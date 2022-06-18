package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
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

func findActiveReplicaSetOnDeployment(
	ctx context.Context,
	depl *appsv1.Deployment,
) (string, string) {
	rsName := ""
	pthVal := ""

	reCreated := regexp.MustCompile(fmt.Sprintf("Created new replica set \"%s-([[:alnum:]]+)\"", depl.GetName()))
	reInProgress := regexp.MustCompile(fmt.Sprintf("ReplicaSet \"%s-([[:alnum:]]+)\" is progressing.", depl.GetName()))
	reReady := regexp.MustCompile(fmt.Sprintf("ReplicaSet \"%s-([[:alnum:]]+)\" has successfully progressed.", depl.GetName()))

	for _, cond := range depl.Status.Conditions {
		if cond.Status != corev1.ConditionTrue && cond.Type != appsv1.DeploymentProgressing {
			continue
		}
		m := []string{}
		if cond.Reason == "NewReplicaSetCreated" {
			m = reCreated.FindStringSubmatch(cond.Message)
		} else if cond.Reason == "NewReplicaSetUpdated" {
			m = reInProgress.FindStringSubmatch(cond.Message)
		} else if cond.Reason == "NewReplicaSetAvailable" {
			m = reReady.FindStringSubmatch(cond.Message)
		}
		if len(m) < 2 {
			continue
		}
		rsName = fmt.Sprintf("%s-%s", depl.GetName(), m[1])
		pthVal = m[1]
		break
	}
	return rsName, pthVal
}

func findPodsAndControllers(
	ctx context.Context,
	cl client.Reader,
	deplKey client.ObjectKey,
) (*appsv1.Deployment, *appsv1.ReplicaSet, []*corev1.Pod, error) {
	l := logf.FromContext(ctx).WithName("findPodsAndControllers")

	depl := &appsv1.Deployment{}
	err := cl.Get(ctx, deplKey, depl)
	if err != nil {
		return nil, nil, nil, err
	}

	rsName, pthVal := findActiveReplicaSetOnDeployment(ctx, depl)

	if rsName == "" {
		return depl, nil, nil, nil
	}

	rs := &appsv1.ReplicaSet{}
	err = cl.Get(ctx, client.ObjectKey{Namespace: deplKey.Namespace, Name: rsName}, rs)
	if err != nil {
		return depl, nil, nil, err
	}

	podsList := &corev1.PodList{}
	if pthVal != rs.GetLabels()["pod-template-hash"] {
		l.Info(fmt.Sprintf("pod-template-hash mismatch, on deploy was %s, on replicaSet is %s", pthVal, rs.GetLabels()["pod-template-hash"]))
	}
	ml := client.MatchingLabels{"pod-template-hash": rs.GetLabels()["pod-template-hash"]}
	err = cl.List(ctx, podsList, client.InNamespace(deplKey.Namespace), ml)
	if err != nil {
		return depl, rs, nil, err
	}
	pods := []*corev1.Pod{}
	for i, _ := range podsList.Items {
		pod := &podsList.Items[i]
		for _, oref := range pod.GetOwnerReferences() {
			if oref.UID != rs.GetUID() {
				continue
			}
			// pods = append(pods, pod.DeepCopy())
			pods = append(pods, pod)
		}
	}
	return depl, rs, pods, nil
}

func hostAliasesNeedsPatch(ctx context.Context, svcIp string, dnsNames []string, hostAliases []corev1.HostAlias) bool {
	l := logf.FromContext(ctx).WithName("needs-patch")
	if len(dnsNames) == 0 || svcIp == "" {
		l.V(3).Info("Skipping check", "dnsNames", dnsNames, "svcIp", svcIp)
		return false
	}
	if len(hostAliases) == 0 {
		l.V(3).Info("Needs patch because no hostAliases")
		return true
	}
	found := false
	for _, hostAlias := range hostAliases {
		if hostAlias.IP != svcIp {
			continue
		}
		hnMap := make(map[string]struct{})
		for _, hn := range hostAlias.Hostnames {
			hnMap[hn] = struct{}{}
		}
		for _, n := range dnsNames {
			if _, ok := hnMap[n]; !ok {
				found = false
				break
			}
			l.V(4).Info("No patch needed, because all hostAliases were found")
			found = true
		}
	}
	if !found {
		return true
	}
	return false
}

func doInitialReconcile(ctx context.Context, cl client.Client, api client.Reader) {
	l := logf.FromContext(ctx)

	svc := &corev1.Service{}
	err := api.Get(ctx, client.ObjectKey{Namespace: svcNamespace, Name: svcName}, svc)
	if err != nil {
		l.Error(err, "Failed to get service", "name", svcName, "ns", svcNamespace)
		return
	}

	findings := make(map[string]struct {
		d    *appsv1.Deployment
		rs   *appsv1.ReplicaSet
		pods []*corev1.Pod
	})

	for _, tgtName := range tgtDeployments {
		depl, rs, pods, err := findPodsAndControllers(ctx, api, client.ObjectKey{Namespace: tgtNamespace, Name: tgtName})
		if err != nil {
			l.Error(err, "Failed to get target deployment", "name", tgtName, "ns", tgtNamespace)
			continue
		}
		// if pods == nil {
		// 	l.Info("No pods found for target deployment", "name", tgtName, "ns", tgtNamespace)
		// 	continue
		// }

		findings[fmt.Sprintf("%s/%s", depl.GetNamespace(), depl.GetName())] = struct {
			d    *appsv1.Deployment
			rs   *appsv1.ReplicaSet
			pods []*corev1.Pod
		}{d: depl, rs: rs, pods: pods}
	}

	svcClusterIpMu.Lock()
	defer svcClusterIpMu.Unlock()
	svcClusterIp = svc.Spec.ClusterIP
	patches := make(map[*appsv1.Deployment]client.Patch)
	patch := []byte(`{"metadata":{"annotations":{"host-aliases-patcher": "needed"}}}`)
	for _, f := range findings {
		if hostAliasesNeedsPatch(ctx, svcClusterIp, dnsNames, f.d.Spec.Template.Spec.HostAliases) {
			// do update using a delayed annotation on deployment
			patches[f.d] = client.RawPatch(types.StrategicMergePatchType, patch)
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

	// initialize Service Cluster IP cache
	{
		svc := &corev1.Service{}
		err := mgr.GetAPIReader().Get(mainCtx, client.ObjectKey{Namespace: svcNamespace, Name: svcName}, svc)
		if err != nil {
			log.Error(err, "Failed to get service", "name", svcName, "ns", svcNamespace)
		}
		svcClusterIpMu.Lock()
		svcClusterIp = svc.Spec.ClusterIP
		svcClusterIpMu.Unlock()
		log.V(2).Info("Service Cluster IP initialized", "name", svc.GetName(), "ip", svcClusterIp)
	}

	err = builder.
		ControllerManagedBy(mgr).
		For(&corev1.Service{}).
		For(&corev1.Pod{}).
		For(&appsv1.ReplicaSet{}).
		For(&appsv1.Deployment{}).
		WithEventFilter(predicate.And(predicate.ResourceVersionChangedPredicate{}, predicate.Funcs{
			CreateFunc: func(evt event.CreateEvent) bool {
				log.V(3).Info("Event filtering (create)", "evt", evt)
				return isWatchedService(evt.Object)
			},
			UpdateFunc: func(evt event.UpdateEvent) bool {
				log.V(3).Info("Event filtering (update)", "evt", evt)
				return isWatchedService(evt.ObjectNew)
			},
		})).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			l := logf.FromContext(ctx).WithName("reconciler")
			cl := mgr.GetClient()

			l.V(3).Info("Procesing", "req", req)

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
			l.V(2).Info("Service ClusterIP (cache) updated", "svcClusterIp", svcClusterIp, "name", svc.GetName())
			if needsNotify {
				// todo: notify that IP is changed/set
				l.V(2).Info("Service ClusterIP (cache) update needs notify")

				patch := []byte(`{"metadata":{"annotations":{"host-aliases-patcher": "needed"}}}`)
				for _, tgtName := range tgtDeployments {
					d := &appsv1.Deployment{}
					if err := cl.Get(ctx, client.ObjectKey{Namespace: tgtNamespace, Name: tgtName}, d); err != nil {
						// just skip
						continue
					}
					err := cl.Patch(ctx, d, client.RawPatch(types.StrategicMergePatchType, patch))
					if err != nil {
						l.Error(err, "Notifying patch failed on deployment", "ns", d.GetNamespace(), "name", d.GetName())
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
		l.Info("Update Service Cluster IP cache", "name", s.GetName(), "ip", s.Spec.ClusterIP)
		svcIp = s.Spec.ClusterIP
	}

	if hostAliasesNeedsPatch(ctx, svcIp, dnsNames, d.Spec.Template.Spec.HostAliases) {
		d.Spec.Template.Spec.HostAliases = append(d.Spec.Template.Spec.HostAliases, corev1.HostAlias{IP: svcIp, Hostnames: dnsNames})
		l.Info("Patching Deployment", "ns", d.GetNamespace(), "name", d.GetName(), "hostAliases", d.Spec.Template.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcIp, "targets", tgtDeployments, "svcClusterIp", svcClusterIp)
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
