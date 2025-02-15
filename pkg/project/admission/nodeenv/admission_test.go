package nodeenv

import (
	"testing"

	"k8s.io/kubernetes/pkg/admission"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/unversioned/testclient"

	projectcache "github.com/openshift/origin/pkg/project/cache"
	"github.com/openshift/origin/pkg/util/labelselector"
)

// TestPodAdmission verifies various scenarios involving pod/project/global node label selectors
func TestPodAdmission(t *testing.T) {
	mockClient := &testclient.Fake{}
	project := &kapi.Namespace{
		ObjectMeta: kapi.ObjectMeta{
			Name:      "testProject",
			Namespace: "",
		},
	}
	projectStore := cache.NewStore(cache.IndexFuncToKeyFuncAdapter(cache.MetaNamespaceIndexFunc))
	projectStore.Add(project)

	handler := &podNodeEnvironment{client: mockClient}
	pod := &kapi.Pod{
		ObjectMeta: kapi.ObjectMeta{Name: "testPod"},
	}

	tests := []struct {
		defaultNodeSelector       string
		projectNodeSelector       string
		podNodeSelector           map[string]string
		mergedNodeSelector        map[string]string
		ignoreProjectNodeSelector bool
		admit                     bool
		testName                  string
	}{
		{
			defaultNodeSelector:       "",
			podNodeSelector:           map[string]string{},
			mergedNodeSelector:        map[string]string{},
			ignoreProjectNodeSelector: true,
			admit:    true,
			testName: "No node selectors",
		},
		{
			defaultNodeSelector:       "infra = false",
			podNodeSelector:           map[string]string{},
			mergedNodeSelector:        map[string]string{"infra": "false"},
			ignoreProjectNodeSelector: true,
			admit:    true,
			testName: "Default node selector and no conflicts",
		},
		{
			defaultNodeSelector: "",
			projectNodeSelector: "infra = false",
			podNodeSelector:     map[string]string{},
			mergedNodeSelector:  map[string]string{"infra": "false"},
			admit:               true,
			testName:            "Project node selector and no conflicts",
		},
		{
			defaultNodeSelector: "infra = false",
			projectNodeSelector: "",
			podNodeSelector:     map[string]string{},
			mergedNodeSelector:  map[string]string{},
			admit:               true,
			testName:            "Empty project node selector and no conflicts",
		},
		{
			defaultNodeSelector: "infra = false",
			projectNodeSelector: "infra=true",
			podNodeSelector:     map[string]string{},
			mergedNodeSelector:  map[string]string{"infra": "true"},
			admit:               true,
			testName:            "Default and project node selector, no conflicts",
		},
		{
			defaultNodeSelector: "infra = false",
			projectNodeSelector: "infra=true",
			podNodeSelector:     map[string]string{"env": "test"},
			mergedNodeSelector:  map[string]string{"infra": "true", "env": "test"},
			admit:               true,
			testName:            "Project and pod node selector, no conflicts",
		},
		{
			defaultNodeSelector: "env = test",
			projectNodeSelector: "infra=true",
			podNodeSelector:     map[string]string{"infra": "false"},
			mergedNodeSelector:  map[string]string{"infra": "false"},
			admit:               false,
			testName:            "Conflicting pod and project node selector, one label",
		},
		{
			defaultNodeSelector: "env=dev",
			projectNodeSelector: "infra=false, env = test",
			podNodeSelector:     map[string]string{"env": "dev", "color": "blue"},
			mergedNodeSelector:  map[string]string{"env": "dev", "color": "blue"},
			admit:               false,
			testName:            "Conflicting pod and project node selector, multiple labels",
		},
	}
	for _, test := range tests {
		cache := projectcache.NewFake(mockClient.Namespaces(), projectStore, test.defaultNodeSelector)
		handler.SetProjectCache(cache)
		if !test.ignoreProjectNodeSelector {
			project.ObjectMeta.Annotations = map[string]string{"openshift.io/node-selector": test.projectNodeSelector}
		}
		pod.Spec = kapi.PodSpec{NodeSelector: test.podNodeSelector}

		err := handler.Admit(admission.NewAttributesRecord(pod, "Pod", "namespace", project.ObjectMeta.Name, "pods", "", admission.Create, nil))
		if test.admit && err != nil {
			t.Errorf("Test: %s, expected no error but got: %s", test.testName, err)
		} else if !test.admit && err == nil {
			t.Errorf("Test: %s, expected an error", test.testName)
		}

		if !labelselector.Equals(test.mergedNodeSelector, pod.Spec.NodeSelector) {
			t.Errorf("Test: %s, expected: %s but got: %s", test.testName, test.mergedNodeSelector, pod.Spec.NodeSelector)
		}
	}
}

func TestHandles(t *testing.T) {
	for op, shouldHandle := range map[admission.Operation]bool{
		admission.Create:  true,
		admission.Update:  false,
		admission.Connect: false,
		admission.Delete:  false,
	} {
		nodeEnvionment, err := NewPodNodeEnvironment(nil)
		if err != nil {
			t.Errorf("%v: error getting node environment: %v", op, err)
			continue
		}

		if e, a := shouldHandle, nodeEnvionment.Handles(op); e != a {
			t.Errorf("%v: shouldHandle=%t, handles=%t", op, e, a)
		}
	}
}
