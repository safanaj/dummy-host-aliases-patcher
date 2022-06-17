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
	l := logf.FromContext(ctx)

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
		l.Info("pod-template-hash mismatch, on deploy was %s, on replicaSet is %s", pthVal, rs.GetLabels()["pod-template-hash"])
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

func anyPodsNeedsForPatch(svcClusterIp string, dnsNames []string, pods []*corev1.Pod) bool {
	if len(dnsNames) == 0 {
		return false
	}

	for _, pod := range pods {
		if len(pod.Spec.HostAliases) == 0 {
			return true
		}
		found := false
		for _, hostAlias := range pod.Spec.HostAliases {
			if hostAlias.IP != svcClusterIp {
				continue
			}
			hnMap := make(map[string]struct{})
			for _, hn := range hostAlias.Hostnames {
				hnMap[hn] = struct{}{}
			}
			for _, n := range dnsNames {
				if _, ok := hnMap[n]; !ok {
					break
				}
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
		if anyPodsNeedsForPatch(svcClusterIp, dnsNames, f.pods) {
			// do update
		}
	}
}

func main() {
	parseFlags()

	if showVersion {
		fmt.Printf("%s version %s\n", progname, version)
		os.Exit(0)
	}

	logf.SetLogger(zap.New())

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

	mgr.AddHealthzCheck("ping", healthz.Ping)
	mgr.AddReadyzCheck("ready", func(_ *http.Request) error {
		objs := []client.Object{&corev1.Service{}, &corev1.Pod{}, &appsv1.ReplicaSet{}, &appsv1.Deployment{}}
		for _, obj := range objs {
			i, err := mgr.GetCache().GetInformer(context.TODO(), obj)
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
		For(&corev1.Service{}).
		For(&corev1.Pod{}).
		For(&appsv1.ReplicaSet{}).
		For(&appsv1.Deployment{}).
		WithEventFilter(predicate.And(predicate.ResourceVersionChangedPredicate{}, predicate.Funcs{
			CreateFunc: func(evt event.CreateEvent) bool {
				return isWatchedService(evt.Object)
			},
			UpdateFunc: func(evt event.UpdateEvent) bool {
				return isWatchedService(evt.ObjectNew)
			},
		})).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			l := logf.FromContext(ctx)
			cl := mgr.GetClient()

			svc := &corev1.Service{}
			err := cl.Get(ctx, req.NamespacedName, svc)
			if err != nil {
				l.Error(err, "Failed to get service")
				return reconcile.Result{}, err
			}
			if !isWatchedService(svc) {
				l.Info("Avoid reconciling other service")
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
			if needsNotify {
				// todo: notify that IP is changed/set
			}
			return reconcile.Result{}, nil
		}))

	err = builder.
		WebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(&hostAliasesDefaulter{}).
		Complete()

	if doInitialReconciliation {
		doInitialReconcile(context.TODO(), mgr.GetClient())
	}

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "could not start manager")
		os.Exit(1)
	}
}

type hostAliasesDefaulter struct {
	client.Client
}

func (a *hostAliasesDefaulter) InjectClient(c client.Client) error {
	a.Client = c
	return nil
}

func (a *hostAliasesDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	l := logf.FromContext(ctx)
	pod, isPod := obj.(*corev1.Pod)

	l.Info("Processing ... ", "pod", pod, "isPod", isPod, "obj", obj)
	if !isPod {
		return fmt.Errorf("expect object to be a %T instead of %T", pod, obj)
	}

	if pod.GetNamespace() != tgtNamespace {
		l.Info("Ignoring pod in wrong namespace", "ns", pod.GetNamespace(), "name", pod.GetName(), "tgtNamespace", tgtNamespace)
		return nil
	}

	tgtNames := make(map[string]struct{})
	for _, tgtName := range tgtDeployments {
		tgtNames[tgtName] = struct{}{}
	}

	pth, ok := pod.GetLabels()["pod-template-hash"]
	if !ok {
		l.Info("Ignoring non controlled pod?", "ns", pod.GetNamespace(), "name", pod.GetName())
		return nil
	}
	deplName := strings.Replace(pod.GetGenerateName(), fmt.Sprintf("-%s-", pth), "", 1)
	if _, ok := tgtNames[deplName]; !ok {
		l.Info("Ignoring wrong named pod", "ns", pod.GetNamespace(), "name", pod.GetName(), "targets", tgtDeployments)
		return nil
	}

	found := false
	for _, oref := range pod.GetOwnerReferences() {
		if oref.Controller != nil && !*oref.Controller {
			l.Info("Ignoring non controlled pod?", "ns", pod.GetNamespace(), "name", pod.GetName(), "oref", oref)
			continue
		}
		rs := &appsv1.ReplicaSet{}
		l.Info("Retrieving RS", "oref", oref, "orefName", oref.Name)
		if err := a.Get(ctx, client.ObjectKey{Namespace: pod.GetNamespace(), Name: oref.Name}, rs); err != nil {
			l.Error(err, "Failed to get owner ReplicaSet", "ns", pod.GetNamespace(), "orefName", oref.Name)
			return err
		}
		for _, oref := range rs.GetOwnerReferences() {
			if oref.Controller != nil && !*oref.Controller {
				l.Info("Ignoring non controlled replicaSet?", "ns", rs.GetNamespace(), "name", rs.Name, "oref", oref)
				continue
			}
			l.Info("Check RS Owner", "oref", oref, "orefName", oref.Name)
			if _, ok := tgtNames[oref.Name]; !ok {
				l.Info("Ignoring wrong named replicaSet owner", "ns", rs.GetNamespace(), "nmae", rs.GetName(), "oref", oref)
				continue
			}
			found = true
		}
	}

	if !found {
		l.Info("Ignoring pod", "ns", pod.GetNamespace(), "name", pod.GetName())
		return nil
	}

	svcClusterIpMu.RLock()
	defer svcClusterIpMu.RUnlock()

	if anyPodsNeedsForPatch(svcClusterIp, dnsNames, []*corev1.Pod{pod}) {
		pod.Spec.HostAliases = append(pod.Spec.HostAliases, corev1.HostAlias{IP: svcClusterIp, Hostnames: dnsNames})
		l.Info("Patching pod", "ns", pod.GetNamespace(), "name", pod.GetName(), "hostAliases", pod.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcClusterIp, "targets", tgtDeployments)
	} else {
		l.Info("Already up-to-date pod", "ns", pod.GetNamespace(), "name", pod.GetName(), "hostAliases", pod.Spec.HostAliases,
			"dnsNames", dnsNames, "svcIp", svcClusterIp, "targets", tgtDeployments)
	}

	return nil
}
