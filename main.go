package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	goflag "flag"
	flag "github.com/spf13/pflag"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

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

func findPodsAndControllers(
	ctx context.Context,
	cl client.Client,
	deplKey client.ObjectKey,
) (*appsv1.Deployment, *appsv1.ReplicaSet, []*corev1.Pod, error) {
	l := logf.FromContext(ctx).WithName("findPodsAndControllers")

	depl := &appsv1.Deployment{}
	err := cl.Get(ctx, deplKey, depl)
	if err != nil {
		return nil, nil, nil, err
	}

	rsName := ""
	pthVal := ""
	re := regexp.MustCompile(fmt.Sprintf("ReplicaSet \"%s-([[:alnum:]]+)\" has successfully progressed.", deplKey.Name))

	for _, cond := range depl.Status.Conditions {
		if !(cond.Reason != "NewReplicaSetAvailable" &&
			cond.Status != corev1.ConditionTrue &&
			cond.Type != appsv1.DeploymentProgressing) {
			continue
		}
		m := re.FindStringSubmatch(cond.Message)
		if len(m) < 2 {
			continue
		}
		rsName = fmt.Sprintf("%s-%s", deplKey.Name, m[1])
		pthVal = m[1]
		break
	}

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

func anyPodsNeedsForPatch(ctx context.Context, svcIp string, dnsNames []string, pods []*corev1.Pod) bool {
	l := logf.FromContext(ctx).WithName("needs-patch")

	if len(dnsNames) == 0 || svcIp == "" {
		l.V(3).Info("Skipping check", "dnsNames", dnsNames, "svcIp", svcIp)
		return false
	}

	for _, pod := range pods {
		if len(pod.Spec.HostAliases) == 0 {
			l.V(3).Info("Needs patch because no hostAliases", "pod", pod)
			return true
		}
		found := false
		for _, hostAlias := range pod.Spec.HostAliases {
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
				l.V(4).Info("No patch needed, because no found all hostAliases")
				found = true
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func doInitialReconcile(ctx context.Context, cl client.Client) {
	l := logf.FromContext(ctx)

	svc := &corev1.Service{}
	err := cl.Get(ctx, client.ObjectKey{Namespace: svcNamespace, Name: svcName}, svc)
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
		depl, rs, pods, err := findPodsAndControllers(ctx, cl, client.ObjectKey{Namespace: tgtNamespace, Name: tgtName})
		if err != nil {
			l.Error(err, "Failed to get target deployment", "name", tgtName, "ns", tgtNamespace)
			continue
		}
		if pods == nil {
			l.Info("No pods found for target deployment", "name", tgtName, "ns", tgtNamespace)
			continue
		}

		findings[fmt.Sprintf("%s/%s", depl.GetNamespace(), depl.GetName())] = struct {
			d    *appsv1.Deployment
			rs   *appsv1.ReplicaSet
			pods []*corev1.Pod
		}{d: depl, rs: rs, pods: pods}
	}

	svcClusterIpMu.Lock()
	defer svcClusterIpMu.Unlock()
	svcClusterIp = svc.Spec.ClusterIP
	for _, f := range findings {
		if anyPodsNeedsForPatch(ctx, svcClusterIp, dnsNames, f.pods) {
			// do update
		}
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
			l.V(2).Info("Service ClusterIP (cache) updated", "svcClusterIp", svcClusterIp, "name", svc.Name())
			if needsNotify {
				// todo: notify that IP is changed/set
				l.V(2).Info("Service ClusterIP (cache) update needs notify")
			}
			return reconcile.Result{}, nil
		}))

	err = builder.
		WebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(&hostAliasesDefaulter{Cache: mgr.GetCache()}).
		Complete()

	if doInitialReconciliation {
		doInitialReconcile(context.TODO(), mgr.GetClient())
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
	l := logf.FromContext(ctx)
	pod, isPod := obj.(*corev1.Pod)

	l.V(3).Info("Processing ... ", "pod", pod.GetName(), "isPod", isPod)
	l.V(4).Info("Details ... ", "obj", obj)
	if !isPod {
		return fmt.Errorf("expect object to be a %T instead of %T", pod, obj)
	}

	if pod.GetNamespace() != tgtNamespace {
		l.V(2).Info("Ignoring pod in wrong namespace", "ns", pod.GetNamespace(), "name", pod.GetName(), "tgtNamespace", tgtNamespace)
		return nil
	}

	tgtNames := make(map[string]struct{})
	for _, tgtName := range tgtDeployments {
		tgtNames[tgtName] = struct{}{}
	}

	pth, ok := pod.GetLabels()["pod-template-hash"]
	if !ok {
		l.V(2).Info("Ignoring non controlled pod?", "ns", pod.GetNamespace(), "name", pod.GetName())
		return nil
	}
	deplName := strings.Replace(pod.GetGenerateName(), fmt.Sprintf("-%s-", pth), "", 1)
	if _, ok := tgtNames[deplName]; !ok {
		l.V(2).Info("Ignoring wrong named pod", "ns", pod.GetNamespace(), "name", pod.GetName(), "targets", tgtDeployments)
		return nil
	}

	found := false
	for _, oref := range pod.GetOwnerReferences() {
		if oref.Controller != nil && !*oref.Controller {
			l.V(2).Info("Ignoring non controlled pod?", "ns", pod.GetNamespace(), "name", pod.GetName(), "oref", oref)
			continue
		}
		rs := &appsv1.ReplicaSet{}
		l.V(3).Info("Retrieving RS", "oref", oref)
		if err := a.Get(ctx, client.ObjectKey{Namespace: pod.GetNamespace(), Name: oref.Name}, rs); err != nil {
			l.Error(err, "Failed to get owner ReplicaSet", "ns", pod.GetNamespace(), "orefName", oref.Name)
			return err
		}
		for _, oref := range rs.GetOwnerReferences() {
			if oref.Controller != nil && !*oref.Controller {
				l.Info("Ignoring non controlled replicaSet?", "ns", rs.GetNamespace(), "name", rs.Name, "oref", oref)
				continue
			}
			l.V(3).Info("Check RS Owner", "oref", oref, "orefName", oref.Name)
			if _, ok := tgtNames[oref.Name]; !ok {
				l.Info("Ignoring wrong named replicaSet owner", "ns", rs.GetNamespace(), "nmae", rs.GetName(), "oref", oref)
				continue
			}
			found = true
		}
	}

	if !found {
		l.V(2).Info("Ignoring pod", "ns", pod.GetNamespace(), "name", pod.GetName())
		return nil
	}

	svcClusterIpMu.RLock()
	svcIp := svcClusterIp
	defer svcClusterIpMu.RUnlock()
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

	l.V(4).Info("Checking pod for host aliases", "ns", pod.GetNamespace(), "name", pod.GetName(), "hostAliases", pod.Spec.HostAliases)
	if anyPodsNeedsForPatch(ctx, svcIp, dnsNames, []*corev1.Pod{pod}) {
		pod.Spec.HostAliases = append(pod.Spec.HostAliases, corev1.HostAlias{IP: svcIp, Hostnames: dnsNames})
		l.Info("Patching pod", "ns", pod.GetNamespace(), "name", pod.GetName(), "hostAliases", pod.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcIp, "targets", tgtDeployments, "svcClusterIp", svcClusterIp)
	} else {
		l.Info("Already up-to-date pod", "ns", pod.GetNamespace(), "name", pod.GetName(), "hostAliases", pod.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcIp, "targets", tgtDeployments, "svcClusterIp", svcClusterIp)
	}

	return nil
}
