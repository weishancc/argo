package controller

import (
	"context"
	"testing"
	"time"

	"github.com/argoproj/pkg/sync"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"

	"github.com/stretchr/testify/assert"
	authorizationv1 "k8s.io/api/authorization/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo/config"
	"github.com/argoproj/argo/persist/sqldb"
	wfv1 "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	fakewfclientset "github.com/argoproj/argo/pkg/client/clientset/versioned/fake"
	"github.com/argoproj/argo/pkg/client/clientset/versioned/scheme"
	wfextv "github.com/argoproj/argo/pkg/client/informers/externalversions"
	"github.com/argoproj/argo/test"
	"github.com/argoproj/argo/workflow/common"
	controllercache "github.com/argoproj/argo/workflow/controller/cache"
	"github.com/argoproj/argo/workflow/controller/estimation"
	"github.com/argoproj/argo/workflow/events"
	hydratorfake "github.com/argoproj/argo/workflow/hydrator/fake"
	"github.com/argoproj/argo/workflow/metrics"
)

var helloWorldWf = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: hello-world
spec:
  entrypoint: whalesay
  templates:
  - name: whalesay
    metadata:
      annotations:
        annotationKey1: "annotationValue1"
        annotationKey2: "annotationValue2"
      labels:
        labelKey1: "labelValue1"
        labelKey2: "labelValue2"
    container:
      image: docker/whalesay:latest
      command: [cowsay]
      args: ["hello world"]
`

var testDefaultWf = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: hello-world
  labels:
    foo: bar
spec:
  entrypoint: whalesay
  serviceAccountName: whalesay
  templates:
  - name: whalesay
    metadata:
      annotations:
        annotationKey1: "annotationValue1"
        annotationKey2: "annotationValue2"
      labels:
        labelKey1: "labelValue1"
        labelKey2: "labelValue2"
    container:
      image: docker/whalesay:latest
      command: [cowsay]
      args: ["hello world"]
`

var testDefaultWfTTL = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: hello-world
spec:
  entrypoint: whalesay
  serviceAccountName: whalesay
  ttlSecondsAfterFinished: 7
  ttlStrategy:
    secondsAfterCompletion: 5
  templates:
  - name: whalesay
    metadata:
      annotations:
        annotationKey1: "annotationValue1"
        annotationKey2: "annotationValue2"
      labels:
        labelKey1: "labelValue1"
        labelKey2: "labelValue2"
    container:
      image: docker/whalesay:latest
      command: [cowsay]
      args: ["hello world"]
`

type testEventRecorderManager struct {
	eventRecorder *record.FakeRecorder
}

func (t testEventRecorderManager) Get(string) record.EventRecorder {
	return t.eventRecorder
}

var _ events.EventRecorderManager = &testEventRecorderManager{}

func newController(objects ...runtime.Object) (context.CancelFunc, *WorkflowController) {
	wfclientset := fakewfclientset.NewSimpleClientset(objects...)
	informerFactory := wfextv.NewSharedInformerFactory(wfclientset, 10*time.Minute)
	wfInformer := cache.NewSharedIndexInformer(nil, nil, 0, nil)
	wftmplInformer := informerFactory.Argoproj().V1alpha1().WorkflowTemplates()
	cwftmplInformer := informerFactory.Argoproj().V1alpha1().ClusterWorkflowTemplates()
	ctx, cancel := context.WithCancel(context.Background())
	go wftmplInformer.Informer().Run(ctx.Done())
	go cwftmplInformer.Informer().Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), wftmplInformer.Informer().HasSynced) {
		panic("Timed out waiting for caches to sync")
	}
	if !cache.WaitForCacheSync(ctx.Done(), cwftmplInformer.Informer().HasSynced) {
		panic("Timed out waiting for caches to sync")
	}
	kube := fake.NewSimpleClientset()
	controller := &WorkflowController{
		Config: config.Config{
			ExecutorImage: "executor:latest",
		},
		kubeclientset:        kube,
		dynamicInterface:     dynamicfake.NewSimpleDynamicClient(scheme.Scheme),
		wfclientset:          wfclientset,
		completedPods:        make(chan string, 16),
		wfInformer:           wfInformer,
		wftmplInformer:       wftmplInformer,
		cwftmplInformer:      cwftmplInformer,
		wfQueue:              workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		podQueue:             workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		workflowKeyLock:      sync.NewKeyLock(),
		wfArchive:            sqldb.NullWorkflowArchive,
		hydrator:             hydratorfake.Noop,
		estimatorFactory:     estimation.DummyEstimatorFactory,
		metrics:              metrics.New(metrics.ServerConfig{}, metrics.ServerConfig{}),
		eventRecorderManager: &testEventRecorderManager{eventRecorder: record.NewFakeRecorder(16)},
		archiveLabelSelector: labels.Everything(),
		cacheFactory:         controllercache.NewCacheFactory(kube, "default"),
	}
	controller.podInformer = controller.newPodInformer()
	return cancel, controller
}

func newControllerWithDefaults() (context.CancelFunc, *WorkflowController) {
	cancel, controller := newController()
	myBool := true
	controller.Config.WorkflowDefaults = &wfv1.Workflow{
		Spec: wfv1.WorkflowSpec{
			HostNetwork: &myBool,
		},
	}
	return cancel, controller
}

func newControllerWithComplexDefaults() (context.CancelFunc, *WorkflowController) {
	cancel, controller := newController()
	myBool := true
	var ten int32 = 10
	var seven int32 = 10
	controller.Config.WorkflowDefaults = &wfv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"annotation": "value",
			},
			Labels: map[string]string{
				"label": "value",
			},
		},
		Spec: wfv1.WorkflowSpec{
			HostNetwork:        &myBool,
			Entrypoint:         "good_entrypoint",
			ServiceAccountName: "my_service_account",
			TTLStrategy: &wfv1.TTLStrategy{
				SecondsAfterCompletion: &ten,
				SecondsAfterSuccess:    &ten,
				SecondsAfterFailure:    &ten,
			},
			TTLSecondsAfterFinished: &seven,
		},
	}
	return cancel, controller
}

func unmarshalWF(yamlStr string) *wfv1.Workflow {
	return test.LoadWorkflowFromBytes([]byte(yamlStr))
}

func unmarshalWFTmpl(yamlStr string) *wfv1.WorkflowTemplate {
	return test.LoadWorkflowTemplateFromBytes([]byte(yamlStr))
}

func unmarshalCWFTmpl(yamlStr string) *wfv1.ClusterWorkflowTemplate {
	return test.LoadClusterWorkflowTemplateFromBytes([]byte(yamlStr))
}

func unmarshalArtifact(yamlStr string) *wfv1.Artifact {
	var artifact wfv1.Artifact
	err := yaml.Unmarshal([]byte(yamlStr), &artifact)
	if err != nil {
		panic(err)
	}
	return &artifact
}

type with func(pod *apiv1.Pod)

func withOutputs(v string) with { return withAnnotation(common.AnnotationKeyOutputs, v) }

func withAnnotation(key, val string) with {
	return func(pod *apiv1.Pod) { pod.Annotations[key] = val }
}

// makePodsPhase acts like a pod controller and simulates the transition of pods transitioning into a specified state
func makePodsPhase(woc *wfOperationCtx, phase apiv1.PodPhase, with ...with) {
	podcs := woc.controller.kubeclientset.CoreV1().Pods(woc.wf.GetNamespace())
	pods, err := podcs.List(metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == "" {
			pod.Status.Phase = phase
			if phase == apiv1.PodFailed {
				pod.Status.Message = "Pod failed"
			}
			for _, w := range with {
				w(&pod)
			}
			updatedPod, err := podcs.Update(&pod)
			if err != nil {
				panic(err)
			}
			err = woc.controller.podInformer.GetStore().Update(updatedPod)
			if err != nil {
				panic(err)
			}
		}
	}
}

func deletePods(woc *wfOperationCtx) {
	for _, obj := range woc.controller.podInformer.GetStore().List() {
		pod := obj.(*apiv1.Pod)
		err := woc.controller.kubeclientset.CoreV1().Pods(pod.Namespace).Delete(pod.Name, nil)
		if err != nil {
			panic(err)
		}
		err = woc.controller.podInformer.GetStore().Delete(obj)
		if err != nil {
			panic(err)
		}
	}
}

func TestAddingWorkflowDefaultValueIfValueNotExist(t *testing.T) {
	ans := true
	t.Run("WithoutDefaults", func(t *testing.T) {
		cancel, controller := newController()
		defer cancel()
		workflow := unmarshalWF(helloWorldWf)
		err := controller.setWorkflowDefaults(workflow)
		assert.NoError(t, err)
		assert.Equal(t, workflow, unmarshalWF(helloWorldWf))
	})
	t.Run("WithDefaults", func(t *testing.T) {
		cancel, controller := newControllerWithDefaults()
		defer cancel()
		defaultWorkflowSpec := unmarshalWF(helloWorldWf)
		err := controller.setWorkflowDefaults(defaultWorkflowSpec)
		assert.NoError(t, err)
		assert.Equal(t, defaultWorkflowSpec.Spec.HostNetwork, &ans)
		assert.NotEqual(t, defaultWorkflowSpec, unmarshalWF(helloWorldWf))
		assert.Equal(t, *defaultWorkflowSpec.Spec.HostNetwork, true)
	})
}

func TestAddingWorkflowDefaultComplex(t *testing.T) {
	cancel, controller := newControllerWithComplexDefaults()
	defer cancel()
	workflow := unmarshalWF(testDefaultWf)
	var ten int32 = 10
	assert.Equal(t, workflow.Spec.Entrypoint, "whalesay")
	assert.Nil(t, workflow.Spec.TTLStrategy)
	assert.Contains(t, workflow.Labels, "foo")
	err := controller.setWorkflowDefaults(workflow)
	assert.NoError(t, err)
	assert.NotEqual(t, workflow, unmarshalWF(testDefaultWf))
	assert.Equal(t, workflow.Spec.Entrypoint, "whalesay")
	assert.Equal(t, workflow.Spec.ServiceAccountName, "whalesay")
	assert.Equal(t, *workflow.Spec.TTLStrategy.SecondsAfterFailure, ten)
	assert.Contains(t, workflow.Labels, "foo")
	assert.Contains(t, workflow.Labels, "label")
	assert.Contains(t, workflow.Annotations, "annotation")
}

func TestAddingWorkflowDefaultComplexTwo(t *testing.T) {
	cancel, controller := newControllerWithComplexDefaults()
	defer cancel()
	workflow := unmarshalWF(testDefaultWfTTL)
	var ten int32 = 10
	var seven int32 = 7
	var five int32 = 5
	err := controller.setWorkflowDefaults(workflow)
	assert.NoError(t, err)
	assert.NotEqual(t, workflow, unmarshalWF(testDefaultWfTTL))
	assert.Equal(t, workflow.Spec.Entrypoint, "whalesay")
	assert.Equal(t, workflow.Spec.ServiceAccountName, "whalesay")
	assert.Equal(t, *workflow.Spec.TTLStrategy.SecondsAfterCompletion, five)
	assert.Equal(t, *workflow.Spec.TTLStrategy.SecondsAfterFailure, ten)
	assert.Equal(t, *workflow.Spec.TTLSecondsAfterFinished, seven)
	assert.NotContains(t, workflow.Labels, "foo")
	assert.Contains(t, workflow.Labels, "label")
	assert.Contains(t, workflow.Annotations, "annotation")
}

func TestNamespacedController(t *testing.T) {
	kubeClient := fake.Clientset{}
	allowed := false
	kubeClient.AddReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed},
		}, nil
	})

	cancel, controller := newController()
	defer cancel()
	controller.kubeclientset = kubernetes.Interface(&kubeClient)
	controller.cwftmplInformer = nil
	controller.createClusterWorkflowTemplateInformer(context.TODO())
	assert.Nil(t, controller.cwftmplInformer)
}

func TestClusterController(t *testing.T) {
	kubeClient := fake.Clientset{}
	allowed := true
	kubeClient.AddReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed},
		}, nil
	})

	cancel, controller := newController()
	defer cancel()
	controller.kubeclientset = kubernetes.Interface(&kubeClient)
	controller.cwftmplInformer = nil
	controller.createClusterWorkflowTemplateInformer(context.TODO())
	assert.NotNil(t, controller.cwftmplInformer)
}

func TestWorkflowController_archivedWorkflowGarbageCollector(t *testing.T) {
	cancel, controller := newController()
	defer cancel()

	controller.archivedWorkflowGarbageCollector(make(chan struct{}))
}

const wfWithTmplRef = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  generateName: workflow-template-hello-world-
  namespace: default
spec:
  entrypoint: whalesay-template
  arguments:
    parameters:
    - name: message
      value: "test"
  workflowTemplateRef:
    name: workflow-template-whalesay-template
`
const wfTmpl = `
apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: workflow-template-whalesay-template
  namespace: default
spec:
  serviceAccountName: my-sa
  priority: 77
  templates:
  - name: whalesay-template
    inputs:
      parameters:
      - name: message
    container:
      image: docker/whalesay
      command: [cowsay]
      args: ["{{inputs.parameters.message}}"]
  volumes:
  - name: data
    empty: {}
`

func TestCheckAndInitWorkflowTmplRef(t *testing.T) {
	wf := unmarshalWF(wfWithTmplRef)
	wftmpl := unmarshalWFTmpl(wfTmpl)
	cancel, controller := newController(wf, wftmpl)
	defer cancel()
	woc := wfOperationCtx{controller: controller,
		wf: wf}
	_, _, err := woc.loadExecutionSpec()
	assert.NoError(t, err)
	assert.Equal(t, wftmpl.Spec.WorkflowSpec.Templates, woc.execWf.Spec.Templates)
}

func TestIsArchivable(t *testing.T) {
	cancel, controller := newController()
	defer cancel()
	var lblSelector metav1.LabelSelector
	lblSelector.MatchLabels = make(map[string]string)
	lblSelector.MatchLabels["workflows.argoproj.io/archive-strategy"] = "true"

	workflow := unmarshalWF(helloWorldWf)
	t.Run("EverythingSelector", func(t *testing.T) {
		controller.archiveLabelSelector = labels.Everything()
		assert.True(t, controller.isArchivable(workflow))
	})
	t.Run("NothingSelector", func(t *testing.T) {
		controller.archiveLabelSelector = labels.Nothing()
		assert.False(t, controller.isArchivable(workflow))
	})
	t.Run("ConfiguredSelector", func(t *testing.T) {
		selector, err := metav1.LabelSelectorAsSelector(&lblSelector)
		assert.NoError(t, err)
		controller.archiveLabelSelector = selector
		assert.False(t, controller.isArchivable(workflow))
		workflow.Labels = make(map[string]string)
		workflow.Labels["workflows.argoproj.io/archive-strategy"] = "true"
		assert.True(t, controller.isArchivable(workflow))
	})
}

func TestReleaseAllWorkflowLocks(t *testing.T) {
	cancel, controller := newController()
	defer cancel()
	t.Run("nilObject", func(t *testing.T) {
		controller.releaseAllWorkflowLocks(nil)
	})
	t.Run("unStructuredObject", func(t *testing.T) {
		un := &unstructured.Unstructured{}
		controller.releaseAllWorkflowLocks(un)
	})
	t.Run("otherObject", func(t *testing.T) {
		un := &wfv1.Workflow{}
		controller.releaseAllWorkflowLocks(un)
	})
}
