package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"k8s.io/client-go/util/retry"

	networkingapi "istio.io/api/networking/v1alpha3"
	networkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ServiceReconciler reconciles headless Services to Istio ServiceEntries
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch;create;update;patch;delete

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("service", req.NamespacedName) // 👈 updated log context
	logger.Info("🔄 Reconciling Service")

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("📉 Service deleted from cluster, checking for ServiceEntry to clean up") // 👈

			se := &networkingv1beta1.ServiceEntry{}
			err := r.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("se-%s", req.Name), Namespace: req.Namespace}, se)
			if err == nil {
				logger.Info("🧹 Deleting orphan ServiceEntry", "serviceentry", se.Name) // 👈
				if err := r.Delete(ctx, se); err != nil && !errors.IsNotFound(err) {
					logger.Error(err, "❌ Failed to delete ServiceEntry", "serviceentry", se.Name) // 👈
					return ctrl.Result{}, err
				}
				logger.Info("✅ Deleted ServiceEntry", "name", se.Name) // 👈
			}
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to fetch Service")
		return ctrl.Result{}, err
	}

	if svc.Spec.ClusterIP != "None" {
		logger.Info("⏭️ Skipping non-headless service", "clusterIP", svc.Spec.ClusterIP) // 👈
		return ctrl.Result{}, nil
	}

	var eps corev1.Endpoints
	if err := r.Get(ctx, req.NamespacedName, &eps); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("⏳ Endpoints not ready, requeuing") // 👈
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		logger.Error(err, "❌ Unable to fetch Endpoints")
		return ctrl.Result{}, err
	}

	var ipList []string
	for _, subset := range eps.Subsets {
		for _, addr := range subset.Addresses {
			ipList = append(ipList, addr.IP)
		}
	}
	if len(ipList) == 0 {
		logger.Info("⚠️ No pod IPs found, skipping ServiceEntry creation")
		return ctrl.Result{}, nil
	}

	var ports []*networkingapi.ServicePort
	for _, p := range svc.Spec.Ports {
		name := p.Name
		istioProto := translateToIstioProtocol(p.Protocol, p.Name, svc.Annotations)

		if name == "" || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(istioProto)) {
			logger.Info("⚙️ Adjusting port name to match Istio protocol", "old", p.Name, "new", istioProto) // 👈
			name = strings.ToLower(istioProto)
		}
		if name == "" {
			name = fmt.Sprintf("port-%d", p.Port)
		}

		ports = append(ports, &networkingapi.ServicePort{
			Name:     name,
			Number:   uint32(p.Port),
			Protocol: istioProto,
		})
	}

	exportTo := "."
	if val, ok := svc.Annotations["istio.headless/exportTo"]; ok && val != "" {
		exportTo = val
		logger.Info("📦 Overriding exportTo from annotation", "exportTo", exportTo) // 👈
	}

	host := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
	logger.Info("🔧 Preparing ServiceEntry", "host", host, "ports", ports, "ips", ipList) // 👈

	se := &networkingv1beta1.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("se-%s", svc.Name),
			Namespace: svc.Namespace,
			Labels: map[string]string{
				"headless": "",
			},
		},
		Spec: networkingapi.ServiceEntry{
			Hosts:      []string{host},
			Location:   networkingapi.ServiceEntry_MESH_INTERNAL,
			Resolution: networkingapi.ServiceEntry_NONE,
			Ports:      ports,
			ExportTo:   []string{exportTo},
			Addresses:  ipList,
			Endpoints:  []*networkingapi.WorkloadEntry{},
		},
	}

	for _, ip := range ipList {
		portMap := map[string]uint32{}
		for _, p := range ports {
			portMap[p.Name] = p.Number
		}
		se.Spec.Endpoints = append(se.Spec.Endpoints, &networkingapi.WorkloadEntry{
			Address: ip,
			Ports:   portMap,
		})
	}

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		existing := &networkingv1beta1.ServiceEntry{}
		if err := r.Get(ctx, types.NamespacedName{Name: se.Name, Namespace: se.Namespace}, existing); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("📘 Creating new ServiceEntry", "name", se.Name) // 👈
				if err := r.Create(ctx, se); err != nil {
					if errors.IsAlreadyExists(err) {
						logger.Info("⚠️ ServiceEntry already exists, will try update") // 👈
						return nil
					}
					logger.Error(err, "❌ Failed to create ServiceEntry")
					return err
				}
				logger.Info("✅ Created ServiceEntry", "host", host)
				return nil
			}
			logger.Error(err, "❌ Failed to fetch existing ServiceEntry for update")
			return err
		}

		if proto.Equal(&existing.Spec, &se.Spec) {
			logger.Info("🔁 No update needed, spec unchanged", "host", host) // 👈
			return nil
		}

		logger.Info("📝 Updating ServiceEntry", "name", se.Name, "host", host) // 👈
		existing.Spec.Hosts = se.Spec.Hosts
		existing.Spec.Location = se.Spec.Location
		existing.Spec.Resolution = se.Spec.Resolution
		existing.Spec.Ports = se.Spec.Ports
		existing.Spec.ExportTo = se.Spec.ExportTo
		existing.Spec.Addresses = se.Spec.Addresses
		existing.Spec.Endpoints = se.Spec.Endpoints

		if err := r.Update(ctx, existing); err != nil {
			logger.Error(err, "❌ Failed to update ServiceEntry")
			return err
		}
		logger.Info("✅ Updated ServiceEntry", "host", host)
		return nil
	})

	if err != nil {
		logger.Error(err, "❌ Final failure during ServiceEntry reconcile")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func translateToIstioProtocol(proto corev1.Protocol, portName string, annotations map[string]string) string {
	if val, ok := annotations["istio.headless/protocol"]; ok && val != "" {
		return strings.ToUpper(val)
	}

	if proto == corev1.ProtocolUDP {
		return "UDP"
	}

	switch strings.ToLower(portName) {
	case "http", "http2", "grpc", "https", "tls", "mongo", "mysql", "redis":
		return strings.ToUpper(portName)
	}

	return "TCP"
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(
			&corev1.Endpoints{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
				return []ctrl.Request{{
					NamespacedName: types.NamespacedName{
						Name:      obj.GetName(),
						Namespace: obj.GetNamespace(),
					},
				}}
			}),
		).
		Named("serviceentry-headless-sync").
		Complete(r)
}
