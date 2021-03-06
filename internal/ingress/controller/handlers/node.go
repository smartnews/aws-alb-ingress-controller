package handlers

import (
	"context"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/tg"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/backend"
	corev1 "k8s.io/api/core/v1"

	"github.com/golang/glog"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations/class"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ handler.EventHandler = (*EnqueueRequestsForNodeEvent)(nil)

type EnqueueRequestsForNodeEvent struct {
	IngressClass string

	Cache cache.Cache
	Store store.Storer
}

// Create is called in response to an create event - e.g. Pod Creation.
func (h *EnqueueRequestsForNodeEvent) Create(e event.CreateEvent, queue workqueue.RateLimitingInterface) {
	node := e.Object.(*corev1.Node)
	if backend.IsNodeSuitableAsTrafficProxy(node) {
		h.enqueueImpactedIngresses(queue)
	}
}

// Delete is called in response to a delete event - e.g. Pod Deleted.
func (h *EnqueueRequestsForNodeEvent) Delete(e event.DeleteEvent, queue workqueue.RateLimitingInterface) {
	node := e.Object.(*corev1.Node)
	if backend.IsNodeSuitableAsTrafficProxy(node) {
		h.enqueueImpactedIngresses(queue)
	}
}

// Update is called in response to an update event -  e.g. Pod Updated.
func (h *EnqueueRequestsForNodeEvent) Update(e event.UpdateEvent, queue workqueue.RateLimitingInterface) {
	nodeOld := e.ObjectOld.(*corev1.Node)
	nodeNew := e.ObjectNew.(*corev1.Node)
	if backend.IsNodeSuitableAsTrafficProxy(nodeOld) != backend.IsNodeSuitableAsTrafficProxy(nodeNew) {
		h.enqueueImpactedIngresses(queue)
	}
}

// Generic is called in response to an event of an unknown type or a synthetic event triggered as a cron or
// external trigger request - e.g. reconcile Autoscaling, or a Webhook.
func (h *EnqueueRequestsForNodeEvent) Generic(event.GenericEvent, workqueue.RateLimitingInterface) {
}

// Ideally this should only enqueue ingresses that have changed
func (h *EnqueueRequestsForNodeEvent) enqueueImpactedIngresses(queue workqueue.RateLimitingInterface) {
	ingressList := &extensions.IngressList{}
	if err := h.Cache.List(context.Background(), nil, ingressList); err != nil {
		glog.Errorf("failed to fetch impacted ingresses by node due to %v", err)
		return
	}

	for _, ingress := range ingressList.Items {
		if !class.IsValidIngress(h.IngressClass, &ingress) {
			continue
		}
		namespacedName := types.NamespacedName{
			Namespace: ingress.Namespace,
			Name:      ingress.Name,
		}
		key := namespacedName.String()
		ingressAnnos, err := h.Store.GetIngressAnnotations(key)
		if err != nil {
			glog.Errorf("failed to get ingress annotations of %s due to %v", key, err)
			return
		}
		if *ingressAnnos.TargetGroup.TargetType == elbv2.TargetTypeEnumInstance {
			queue.Add(reconcile.Request{
				NamespacedName: namespacedName,
			})
			continue
		}

		backends, _, err := tg.ExtractTargetGroupBackends(&ingress)
		for _, backend := range backends {
			service := &corev1.Service{}
			nspname := types.NamespacedName{
				Namespace: ingress.Namespace,
				Name:      backend.ServiceName,
			}
			if err := h.Cache.Get(context.Background(), nspname, service); err != nil {
				glog.Errorf("failed to fetch service %s backing ingress %s/%s, ignoring",
					backend.ServiceName, ingress.Namespace, ingress.Name)
				continue
			}
			if isNodePortService(service) {
				queue.Add(reconcile.Request{
					NamespacedName: namespacedName,
				})
				break
			}
		}
	}
}

func isNodePortService(service *corev1.Service) bool {
	return service.Spec.Type == corev1.ServiceTypeNodePort
}
