package node

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	Namespace string
)

// Add creates a new Node Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileNode{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {

	// Create a new controller
	c, err := controller.New("node-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Node
	err = c.Watch(&source.Kind{Type: &v1.Node{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileNode implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileNode{}

// ReconcileNode reconciles a Node object
type ReconcileNode struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Node object and makes changes based on the state read
// and what is in the Node.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileNode) Reconcile(request reconcile.Request) (reconcile.Result, error) {

	// Fetch the Node instance
	node := &v1.Node{}
	err := r.client.Get(context.TODO(), request.NamespacedName, node)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Get fencing status of the node
	fencingState := node.Annotations["fencing/state"]

	// Get node condition
	_, c := GetNodeCondition(&node.Status, v1.NodeReady)
	if c == nil {
		return reconcile.Result{}, nil
	}

	// Node is Ready
	if c.Status == v1.ConditionTrue {

		// Node recovered, remove fencing/state annotation
		if fencingState == "pending" || fencingState == "fenced" {
			klog.Infoln("Node", node.Name, "recovered")
			mergePatch, _ := json.Marshal(map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{
						"fencing/state":     nil,
						"fencing/timestamp": nil,
					},
				},
			})
			err = r.client.Patch(context.TODO(), node, client.RawPatch(types.MergePatchType, mergePatch))
			if err != nil {
				klog.Errorln("Failed to patch node", node.Name, ":", err)
			}
			fencingState = "recovered"
		}
	}

	// Ignore already fenced nodes
	if fencingState == "fenced" {
		return reconcile.Result{}, nil
	}

	// We need only nodes with Unknown status
	if fencingState != "recovered" && c.Reason != "NodeStatusUnknown" {
		return reconcile.Result{}, nil
	}

	// Get fencing template name
	templateName, ok := node.Annotations["fencing/template"]
	if !ok {
		templateName = "fencing"
	}

	// Find PodTemplate
	podTemplate := &v1.PodTemplate{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: templateName, Namespace: Namespace}, podTemplate)
	if err != nil && errors.IsNotFound(err) {
		klog.Errorln("Failed to find podTemplate", templateName, ":", err)
		return reconcile.Result{}, nil
	}

	// Define a new Job object
	job := newJobForNode(node, podTemplate)

	// Remove previous fencing job
	if fencingState == "recovered" {
		// Check if this Job already exists
		found := &batchv1.Job{}
		err = r.client.Get(context.TODO(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, found)
		if err == nil {
			klog.Infoln("Deleting job", job.Name)
			err = r.client.Delete(context.TODO(), found)
			if err != nil {
				klog.Errorln("Failed to delete job", job.Name, ":", err)
			}
		}
		return reconcile.Result{}, nil
	}

	// Handle only nodes with fencing/enabled=true annotation
	if node.Annotations["fencing/enabled"] != "true" {
		return reconcile.Result{}, nil
	}

	// ======================================
	// Fencing procedure is not started yet
	// ======================================

	if fencingState != "started" {

		// Get timeout period from annotation
		timeoutStr, ok := node.Annotations["fencing/timeout"]
		if !ok {
			timeoutStr, ok = podTemplate.Annotations["fencing/timeout"]
			if !ok {
				timeoutStr = "0"
			}
		}
		timeout, err := strconv.Atoi(timeoutStr)
		if err != nil {
			klog.Errorln("Failed to parse timeout string", timeoutStr, ":", err)
			return reconcile.Result{}, nil
		}

		// If timeout specified, then set fencing/status=delayed and wait timeout
		if timeout > 0 {
			fencingTimestampStr, _ := node.Annotations["fencing/timestamp"]
			fencingTimestamp, _ := strconv.ParseInt(fencingTimestampStr, 10, 64)

			// If no timestamp, set it
			if fencingState == "" && fencingTimestamp == 0 {
				// Recording new fecning/started annotation
				fencingTimestamp = time.Now().Unix()
				fencingTimestampStr := strconv.FormatInt(fencingTimestamp, 10)

				mergePatch, _ := json.Marshal(map[string]interface{}{
					"metadata": map[string]interface{}{
						"annotations": map[string]interface{}{
							"fencing/state":     "pending",
							"fencing/timestamp": fencingTimestampStr,
						},
					},
				})

				err = r.client.Patch(context.TODO(), node, client.RawPatch(types.MergePatchType, mergePatch))
				if err != nil {
					klog.Errorln("Failed to patch node", node.Name, ":", err)
					return reconcile.Result{}, err
				}
			}

			// Check remainTime
			remainTime := int64(timeout) - (time.Now().Unix() - fencingTimestamp)
			if remainTime > 0 {
				go func() {
					klog.Infoln("Waiting", remainTime, "seconds, if", node.Name, "comes back online")
					time.Sleep(time.Duration(remainTime) * time.Second)
					// remove annotation after timeout expired
					mergePatch, _ := json.Marshal(map[string]interface{}{
						"metadata": map[string]interface{}{
							"annotations": map[string]interface{}{
								"fencing/timestamp": nil,
							},
						},
					})
					_ = r.client.Patch(context.TODO(), node, client.RawPatch(types.MergePatchType, mergePatch))
				}()

				return reconcile.Result{}, nil
			}
		}

		mergePatch, _ := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					"fencing/state":     "started",
					"fencing/timestamp": nil,
				},
			},
		})
		err = r.client.Patch(context.TODO(), node, client.RawPatch(types.MergePatchType, mergePatch))
		if err != nil {
			klog.Errorln("Failed to patch node", node.Name, ":", err)
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	// ======================================
	// Fencing procedure started
	// ======================================

	// Check if this Job already exists
	found := &batchv1.Job{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, found)
	if err != nil && !errors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	if err != nil {
		// Previus job is not found
		klog.Infoln("Starting fencing", node.Name)
	} else {
		// Previus job is found
		klog.Infoln("Continue fencing", node.Name)

		// Check is job finished
		_, jc := GetJobCondition(&found.Status, batchv1.JobComplete)
		_, jf := GetJobCondition(&found.Status, batchv1.JobFailed)
		if jc == nil && jf == nil {
			// Job is still running - don't requeue
			klog.Infoln("Job", job.Name, "is still running")
			return reconcile.Result{}, nil
		}

		// Old job finished already - remove it
		klog.Infoln("Deleting previous job", job.Name) // TODO: wait for deletion
		err = r.client.Delete(context.TODO(), found)
		if err != nil {
			klog.Errorln("Failed to delete job", job.Name, ":", err)
			return reconcile.Result{}, err
		}
	}

	klog.Infoln("Creating a new job", job.Name)
	err = r.client.Create(context.TODO(), job)
	if err != nil {
		klog.Errorln("Failed to create new job", job.Name, ":", err)
		return reconcile.Result{}, err
	}

	// Job created successfully - don't requeue
	return reconcile.Result{}, nil

}

// newJobForNode returns a Job to fence the node
func newJobForNode(node *v1.Node, podTemplate *v1.PodTemplate) *batchv1.Job {
	labels := map[string]string{
		"node":    node.Name,
		"fencing": "fence",
	}
	// Default annotations
	annotations := map[string]string{
		"fencing/mode":     "flush",
		"fencing/template": "fencing",
		"fencing/timeout":  "0",
	}

	// Override default annotations with podTemplate annotations
	for k, _ := range annotations {
		if v, ok := podTemplate.Annotations[k]; ok {
			annotations[k] = v
		}
		if v, ok := node.Annotations[k]; ok {
			annotations[k] = v
		}
	}

	// Create new pod from podTemplate
	pod := podTemplate.Template

	// Append pod annotations with podTemplate.Template annotations
	if pod.Annotations != nil {
		for k, v := range pod.Annotations {
			annotations[k] = v
		}
	}

	// Append pod annotations with fencing/node and fencing/id annotations
	annotations["fencing/node"] = node.Name
	if id, ok := node.Annotations["fencing/id"]; ok {
		annotations["fencing/id"] = id
	} else if id, ok = podTemplate.Annotations["fencing/id"]; ok {
		annotations["fencing/id"] = id
	} else {
		annotations["fencing/id"] = node.Name
	}
	if afterHook, ok := node.Annotations["fencing/after-hook"]; ok {
		annotations["fencing/after-hook"] = afterHook
	}
	if afterHook, ok := podTemplate.Annotations["fencing/after-hook"]; ok {
		annotations["fencing/after-hook"] = afterHook
	}

	// Apply annotations to the pod
	pod.ObjectMeta.Annotations = annotations

	// Set prefix name
	prefix := pod.Name
	if prefix == "" {
		prefix = "fence"
	}

	// Creating new Job
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        prefix + "-" + node.Name,
			Namespace:   Namespace,
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{
				metav1.OwnerReference{
					APIVersion: node.APIVersion,
					Kind:       node.Kind,
					Name:       node.Name,
					UID:        node.UID,
				},
			},
		},
		Spec: batchv1.JobSpec{
			Template: pod},
	}
}

// GetNodeCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetNodeCondition(status *v1.NodeStatus, conditionType v1.NodeConditionType) (int, *v1.NodeCondition) {
	if status == nil {
		return -1, nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			return i, &status.Conditions[i]
		}
	}
	return -1, nil
}

// GetJobCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetJobCondition(status *batchv1.JobStatus, conditionType batchv1.JobConditionType) (int, *batchv1.JobCondition) {
	if status == nil {
		return -1, nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			return i, &status.Conditions[i]
		}
	}
	return -1, nil
}
