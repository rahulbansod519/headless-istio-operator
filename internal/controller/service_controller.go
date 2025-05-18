package controller

import (
	"context"
	"fmt"
	"time"

	networkingapi "istio.io/api/networking/v1alpha3"
	networkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ServiceReconciler reconciles a headless Service into an Istio ServiceEntry
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch;create;update;patch

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// 1. Get the Service object
	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Service was deleted, clean up the corresponding ServiceEntry
			logger.Info("Service deleted, cleaning up ServiceEntry", "name", req.Name, "namespace", req.Namespace)

			// Create a dummy ServiceEntry object to delete it by name
			se := &networkingv1beta1.ServiceEntry{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("se-%s", req.Name),
					Namespace: req.Namespace,
				},
			}

			if delErr := r.Delete(ctx, se); delErr != nil && !errors.IsNotFound(delErr) {
				logger.Error(delErr, "Failed to delete ServiceEntry for deleted Service")
				return ctrl.Result{}, delErr
			}

			logger.Info("Deleted ServiceEntry for deleted Service", "name", req.Name)
			return ctrl.Result{}, nil
		}

		// If other error, just return
		logger.Error(err, "Failed to fetch Service")
		return ctrl.Result{}, err
	}

	// 2. Skip non-headless services
	if svc.Spec.ClusterIP != "None" {
		logger.Info("Skipping non-headless service", "name", svc.Name, "namespace", svc.Namespace)
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling headless service", "name", svc.Name, "namespace", svc.Namespace)

	// 3. Get the Endpoints associated with the Service
	var eps corev1.Endpoints
	if err := r.Get(ctx, req.NamespacedName, &eps); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("No Endpoints found yet for headless service, requeuing", "name", svc.Name, "namespace", svc.Namespace)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		logger.Error(err, "failed to get Endpoints for headless service")
		return ctrl.Result{}, err
	}

	// 4. Extract pod IPs
	var ipList []string
	for _, subset := range eps.Subsets {
		for _, addr := range subset.Addresses {
			ipList = append(ipList, addr.IP)
		}
	}

	if len(ipList) == 0 {
		logger.Info("No pod IPs found for headless service", "name", svc.Name)
		return ctrl.Result{}, nil
	}

	logger.Info("Discovered pod IPs", "IPs", ipList)

	// 5. Construct ServiceEntry resource
	host := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
	se := &networkingv1beta1.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("se-%s", svc.Name),
			Namespace: svc.Namespace,
		},
		Spec: networkingapi.ServiceEntry{
			Hosts:      []string{host},
			Location:   networkingapi.ServiceEntry_MESH_INTERNAL,
			Resolution: networkingapi.ServiceEntry_NONE,
			Ports: []*networkingapi.ServicePort{ // ✅ fixed
				{
					Number:   8080,
					Name:     "http",
					Protocol: "HTTP",
				},
			},
			Endpoints: []*networkingapi.WorkloadEntry{}, // empty for now
		},
	}

	// 6. Add discovered IPs to ServiceEntry endpoints
	for _, ip := range ipList {
		se.Spec.Endpoints = append(se.Spec.Endpoints, &networkingapi.WorkloadEntry{
			Address: ip,
		})
	}

	// 7. Create or update the ServiceEntry in the cluster
	existing := &networkingv1beta1.ServiceEntry{}
	err := r.Get(ctx, client.ObjectKeyFromObject(se), existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, se); err != nil {
			logger.Error(err, "failed to create ServiceEntry")
			return ctrl.Result{}, err
		}
		logger.Info("Created new ServiceEntry", "host", host)
	} else if err == nil {
		// Field-by-field update (no struct assignment)
		existing.Spec.Hosts = se.Spec.Hosts
		existing.Spec.Ports = se.Spec.Ports
		existing.Spec.Endpoints = se.Spec.Endpoints
		existing.Spec.Location = se.Spec.Location
		existing.Spec.Resolution = se.Spec.Resolution

		if err := r.Update(ctx, existing); err != nil {
			logger.Error(err, "failed to update ServiceEntry")
			return ctrl.Result{}, err
		}
		logger.Info("Updated existing ServiceEntry", "host", host)
	} else {
		logger.Error(err, "failed to get ServiceEntry")
		return ctrl.Result{}, err
	}

	// Done
	return ctrl.Result{}, nil
}

// SetupWithManager registers this controller to watch Service objects
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Named("service").
		Complete(r)
}
