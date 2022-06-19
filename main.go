package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	goflag "flag"
	flag "github.com/spf13/pflag"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	progname    string = "host-aliases-patcher"
	version     string
	showVersion bool = false

	doInitialReconciliation bool = false

	dnsNames       []string
	svcName        string
	svcNamespace   string = ""
	tgtDeployments []string
	tgtNamespace   string = ""

	svcClusterIp   string
	svcClusterIpMu sync.RWMutex
)

func parseFlags() {
	flag.BoolVar(&showVersion, "version", showVersion, "Print version and exit")

	flag.BoolVar(&doInitialReconciliation, "do-initial-reconciliation", doInitialReconciliation, "Perform initial reconciliation")
	flag.StringSliceVar(&dnsNames, "dns-names", []string{}, "Comma seprated DNS names to mask")
	flag.StringVar(&svcName, "cluster-ip-from-service-name", "", "")
	flag.StringVar(&svcNamespace, "cluster-ip-from-service-namespace", "", "")
	flag.StringSliceVar(&tgtDeployments, "target-deployments", []string{}, "")
	flag.StringVar(&tgtNamespace, "target-namespace", "", "")
	flag.CommandLine.AddGoFlag(goflag.Lookup("kubeconfig"))
	flag.Parse()
}

func isWatchedService(o client.Object) bool {
	gvk := o.GetObjectKind().GroupVersionKind()
	objk := client.ObjectKeyFromObject(o)

	return (gvk.Group == "" &&
		gvk.Version == "v1" &&
		gvk.Kind == "Service" &&
		objk.Namespace == svcNamespace &&
		objk.Name == svcName)
}

// exported just for testing with ginkgo
func GetDesiredHostAliases(ctx context.Context, svcIp string, dnsNames []string, hostAliases []corev1.HostAlias) ([]corev1.HostAlias, bool) {
	l := logf.FromContext(ctx).WithName("needs-patch")

	if len(dnsNames) == 0 || svcIp == "" {
		l.V(3).Info("Skipping check", "dnsNames", dnsNames, "svcIp", svcIp)
		return hostAliases, false
	}

	needsUpdate := false
	newHostAliases := []corev1.HostAlias{}
	dnsNamesMap := make(map[string]struct{})
	aliases := make(map[string]map[string]struct{})

	for _, dnsName := range dnsNames {
		dnsNamesMap[dnsName] = struct{}{}
	}

	// keep current hostAliases
	for _, ha := range hostAliases {
		if _, ok := aliases[ha.IP]; !ok {
			aliases[ha.IP] = make(map[string]struct{})
		}
		for _, hn := range ha.Hostnames {
			// skip matching desired dns names, we will add them later
			// in this way we manage wrong IPs associated with dns names, cleaning wrong entries
			if _, skipIt := dnsNamesMap[hn]; !skipIt || ha.IP == svcIp {
				aliases[ha.IP][hn] = struct{}{}
			} else {
				needsUpdate = true
			}
		}
	}

	if _, ok := aliases[svcIp]; !ok {
		aliases[svcIp] = make(map[string]struct{})
		needsUpdate = true
	}

	lenBeforeDnsNames := len(aliases[svcIp])

	for dnsName, _ := range dnsNamesMap {
		aliases[svcIp][dnsName] = struct{}{}
	}

	if !needsUpdate {
		needsUpdate = len(aliases[svcIp]) != lenBeforeDnsNames
	}

	for ip, hosts := range aliases {
		hostnames := make([]string, 0, len(hosts))
		for host, _ := range hosts {
			hostnames = append(hostnames, host)
		}
		if len(hostnames) > 0 {
			newHostAliases = append(newHostAliases, corev1.HostAlias{IP: ip, Hostnames: hostnames})
		} else {
			// needs update for skipped entry
			needsUpdate = true
		}
	}
	return newHostAliases, needsUpdate
}

func doInitialReconcile(ctx context.Context, cl client.Client, api client.Reader) {
	l := logf.FromContext(ctx).WithName("initialReconciler")

	svc := &corev1.Service{}
	err := api.Get(ctx, client.ObjectKey{Namespace: svcNamespace, Name: svcName}, svc)
	if err != nil {
		l.Error(err, "Failed to get service", "name", svcName, "ns", svcNamespace)
		return
	}

	deployments := []*appsv1.Deployment{}
	for _, tgtName := range tgtDeployments {
		depl := &appsv1.Deployment{}
		err := api.Get(ctx, client.ObjectKey{Namespace: tgtNamespace, Name: tgtName}, depl)
		if err != nil {
			l.Error(err, "Failed to get deployment", "name", svcName, "ns", svcNamespace)
			continue
		}
		deployments = append(deployments, depl)
	}

	svcClusterIpMu.Lock()
	defer svcClusterIpMu.Unlock()
	svcClusterIp = svc.Spec.ClusterIP

	patches := make(map[*appsv1.Deployment]client.Patch)
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"host-aliases-patcher": "needed at %s"}}}`, time.Now()))

	for _, d := range deployments {
		_, needsUpdate := GetDesiredHostAliases(ctx, svcClusterIp, dnsNames, d.Spec.Template.Spec.HostAliases)
		if needsUpdate {
			// do update using a delayed annotation on deployment
			patches[d] = client.RawPatch(types.StrategicMergePatchType, patch)
		}
	}

	if len(patches) > 0 {
		time.AfterFunc(time.Second*15, func() {
			for d, rawPatch := range patches {
				err := cl.Patch(ctx, d, rawPatch)
				if err != nil {
					l.Error(err, "Delayed patch failed on deployment", "ns", d.GetNamespace(), "name", d.GetName())
				}
			}
		})
	}
}

func main() {
	opts := zap.Options{}
	{
		fs := goflag.NewFlagSet("", 0)
		opts.BindFlags(fs)
		flag.CommandLine.AddGoFlagSet(fs)
	}
	parseFlags()

	if showVersion {
		fmt.Printf("%s version %s\n", progname, version)
		os.Exit(0)
	}

	logf.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	log := logf.Log.WithName(progname)
	podNs := os.Getenv("POD_NAMESPACE")

	if svcNamespace == "" {
		if podNs == "" {
			log.Error(fmt.Errorf("Missing POD_NAMESPACE environment variable"), "could not identify namespace to watch")
			os.Exit(1)
		}
		svcNamespace = podNs
	}

	if tgtNamespace == "" {
		if podNs == "" {
			log.Error(fmt.Errorf("Missing POD_NAMESPACE environment variable"), "could not identify namespace to watch")
			os.Exit(1)
		}
		tgtNamespace = podNs
	}

	mgrOpts := manager.Options{Logger: log.WithName("mgr")}
	if tgtNamespace == svcNamespace {
		mgrOpts.Namespace = tgtNamespace
	}

	mgr, err := manager.New(config.GetConfigOrDie(), mgrOpts)
	if err != nil {
		log.Error(err, "could not create manager")
		os.Exit(1)
	}

	mainCtx := signals.SetupSignalHandler()
	mgr.AddHealthzCheck("ping", healthz.Ping)
	mgr.AddReadyzCheck("ready", func(_ *http.Request) error {
		objs := []client.Object{&corev1.Service{}, &corev1.Pod{}, &appsv1.ReplicaSet{}, &appsv1.Deployment{}}
		for _, obj := range objs {
			i, err := mgr.GetCache().GetInformer(mainCtx, obj)
			if err != nil {
				return err
			}
			if !i.HasSynced() {
				return fmt.Errorf("%T informer not in sync", obj)
			}
		}
		return nil
	})

	err = builder.
		ControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}, predicate.Funcs{
			CreateFunc: func(evt event.CreateEvent) bool {
				log.V(4).Info("Event filtering (create)", "evt", evt)
				watched := isWatchedService(evt.Object)
				if watched {
					log.Info("Watched service created", "ns", evt.Object.GetNamespace(), "name", evt.Object.GetName())
				}
				return watched
			},
			UpdateFunc: func(evt event.UpdateEvent) bool {
				log.V(4).Info("Event filtering (update)", "evt", evt)
				watched := isWatchedService(evt.ObjectNew)
				if watched {
					log.Info("Watched service updated", "ns", evt.ObjectNew.GetNamespace(), "name", evt.ObjectNew.GetName())
				}
				return watched
			},
		})).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			l := logf.FromContext(ctx).WithName("reconciler")
			cl := mgr.GetClient()

			l.V(2).Info("Procesing", "req", req)

			svc := &corev1.Service{}
			err := cl.Get(ctx, req.NamespacedName, svc)
			if err != nil {
				l.Error(err, "Failed to get service")
				return reconcile.Result{}, err
			}
			if !isWatchedService(svc) {
				l.Info("Avoid reconciling other service", "name", svc.GetName())
				return reconcile.Result{}, nil
			}

			svcClusterIpMu.RLock()
			needsUpdate := svcClusterIp != svc.Spec.ClusterIP
			needsNotify := svcClusterIp != "" || svc.Spec.ClusterIP == "" // not the first time
			svcClusterIpMu.RUnlock()

			if !needsUpdate {
				return reconcile.Result{}, nil
			}
			svcClusterIpMu.Lock()
			defer svcClusterIpMu.Unlock()
			svcClusterIp = svc.Spec.ClusterIP
			l.Info("Service ClusterIP (cache) updated", "svcClusterIp", svcClusterIp, "name", svc.GetName())

			if needsNotify {
				// todo: notify that IP is changed/set
				l.Info("Service ClusterIP (cache) update needs notify")

				patches := make(map[*appsv1.Deployment]client.Patch)
				patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"host-aliases-patcher": "needed at %s"}}}`, time.Now()))
				for _, tgtName := range tgtDeployments {
					d := &appsv1.Deployment{}
					if err := cl.Get(ctx, client.ObjectKey{Namespace: tgtNamespace, Name: tgtName}, d); err != nil {
						// just skip
						continue
					}
					patches[d] = client.RawPatch(types.StrategicMergePatchType, patch)
				}

				if len(patches) > 0 {
					if svcClusterIp != "" {
						for d, rawPatch := range patches {
							l.Info("Notifying deployment for svc cluster IP update", "ns", d.GetNamespace(), "name", d.GetName())
							if err := cl.Patch(ctx, d, rawPatch); err != nil {
								l.Error(err, "Notifying patch failed on deployment", "ns", d.GetNamespace(), "name", d.GetName())
							}
						}
					} else {
						l.Info("Requeue Service because empty IP", "ns", svc.GetNamespace(), "name", svc.GetName())
						return reconcile.Result{RequeueAfter: time.Second * 5}, nil
					}
				}
			}
			return reconcile.Result{}, nil
		}))

	wh := &hostAliasesDefaulter{Cache: mgr.GetCache()}
	err = builder.
		WebhookManagedBy(mgr).
		For(&appsv1.Deployment{}).
		WithDefaulter(wh).
		Complete()

	if doInitialReconciliation {
		doInitialReconcile(mainCtx, mgr.GetClient(), mgr.GetAPIReader())
	}

	if err := mgr.Start(mainCtx); err != nil {
		log.Error(err, "could not start manager")
		os.Exit(1)
	}
}

type hostAliasesDefaulter struct {
	cache.Cache
}

func (a *hostAliasesDefaulter) InjectCache(c cache.Cache) error {
	a.Cache = c
	return nil
}

func (a *hostAliasesDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	l := logf.FromContext(ctx).WithName("defaulter")
	l.V(4).Info("Details ... ", "obj", obj)
	switch objT := obj.(type) {
	case *appsv1.Deployment:
		return a.processDeploy(ctx, objT)
	default:
		return fmt.Errorf("expect object to be a %T or %T instead of %T", &corev1.Pod{}, &appsv1.ReplicaSet{}, obj)
	}
}

func (a *hostAliasesDefaulter) processDeploy(ctx context.Context, d *appsv1.Deployment) error {
	l := logf.FromContext(ctx).WithName("defaulterDeploy")

	l.V(3).Info("Processing Deployment", "ns", d.GetNamespace(), "name", d.GetName())
	if d.GetNamespace() != tgtNamespace {
		l.V(2).Info("Ignoring Deployment in wrong namespace", "ns", d.GetNamespace(), "name", d.GetName(), "tgtNamespace", tgtNamespace)
		return nil
	}

	tgtNames := make(map[string]struct{})
	for _, tgtName := range tgtDeployments {
		tgtNames[tgtName] = struct{}{}
	}
	if _, ok := tgtNames[d.GetName()]; !ok {
		l.V(2).Info("Ignoring Deployment", "ns", d.GetNamespace(), "name", d.GetName(), "tgtDeployments", tgtDeployments)
		return nil
	}

	svcClusterIpMu.RLock()
	svcIp := svcClusterIp
	svcClusterIpMu.RUnlock()
	if svcIp == "" {
		s := &corev1.Service{}
		err := a.Get(ctx, client.ObjectKey{Namespace: svcNamespace, Name: svcName}, s)
		if err != nil {
			l.Error(err, "Failed to get service", "name", svcName, "ns", svcNamespace)
			return err
		}
		l.Info("Service Cluster IP cache not up-to-date, was empty", "name", s.GetName(), "ip", s.Spec.ClusterIP)
		svcIp = s.Spec.ClusterIP
	}

	newHostAliases, needsUpdate := GetDesiredHostAliases(ctx, svcIp, dnsNames, d.Spec.Template.Spec.HostAliases)
	if needsUpdate {
		d.Spec.Template.Spec.HostAliases = newHostAliases
		l.Info("Patching Deployment", "ns", d.GetNamespace(), "name", d.GetName(), "hostAliases", d.Spec.Template.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcIp, "targets", tgtDeployments, "svcClusterIp", svcClusterIp)

		// we changed the deployment, just annotate it
		if d.ObjectMeta.Annotations == nil {
			d.ObjectMeta.Annotations = make(map[string]string)
		}
		d.ObjectMeta.Annotations["host-aliases-patched"] = "dummy"

		if d.Spec.Template.ObjectMeta.Annotations == nil {
			d.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		d.Spec.Template.ObjectMeta.Annotations["host-aliases-patched"] = "dummy"

		delete(d.ObjectMeta.Annotations, "host-aliases-patcher")

	} else {
		l.Info("Already up-to-date Deployment", "ns", d.GetNamespace(), "name", d.GetName(), "hostAliases", d.Spec.Template.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcIp, "targets", tgtDeployments, "svcClusterIp", svcClusterIp)
	}

	return nil
}
