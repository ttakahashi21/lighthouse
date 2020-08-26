package controller_test

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/submariner-io/admiral/pkg/fake"
	"github.com/submariner-io/admiral/pkg/syncer/broker"
	"github.com/submariner-io/admiral/pkg/syncer/test"
	"github.com/submariner-io/lighthouse/pkg/agent/controller"
	lighthousev2a1 "github.com/submariner-io/lighthouse/pkg/apis/lighthouse.submariner.io/v2alpha1"
	lighthouseClientset "github.com/submariner-io/lighthouse/pkg/client/clientset/versioned"
	fakeLighthouseClientset "github.com/submariner-io/lighthouse/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	fakeKubeClient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/testing"
)

const clusterID = "east"

var _ = Describe("ServiceImport syncing", func() {
	t := newTestDiver()

	When("a ServiceExport is created", func() {
		When("the Service already exists", func() {
			It("should correctly sync a ServiceImport and update the ServiceExport status", func() {
				t.createService()
				t.createServiceExport()
				t.awaitServiceExported(t.service.Spec.ClusterIP, 0)
			})
		})

		When("the Service doesn't initially exist", func() {
			It("should initially update the ServiceExport status to Initialized and eventually sync a ServiceImport", func() {
				t.createServiceExport()
				t.awaitServiceUnavailableStatus(0)

				t.createService()
				t.awaitServiceExported(t.service.Spec.ClusterIP, 1)
			})
		})
	})

	When("a ServiceExport is deleted after a ServiceImport is synced", func() {
		It("should delete the ServiceImport", func() {
			t.createService()
			t.createServiceExport()
			t.awaitServiceExported(t.service.Spec.ClusterIP, 0)

			t.deleteServiceExport()
			t.awaitNoServiceImport(t.brokerServiceImportClient)
			t.awaitNoServiceImport(t.localServiceImportClient)
		})
	})

	When("an exported Service is deleted while the ServiceExport still exists", func() {
		It("should delete the ServiceImport", func() {
			t.createService()
			t.createServiceExport()
			nextStatusIndex := t.awaitServiceExported(t.service.Spec.ClusterIP, 0)

			t.deleteService()
			t.awaitNoServiceImport(t.brokerServiceImportClient)
			t.awaitNoServiceImport(t.localServiceImportClient)
			t.awaitServiceUnavailableStatus(nextStatusIndex)
		})
	})

	When("the ServiceImport sync initially fails", func() {
		BeforeEach(func() {
			t.brokerServiceImportClient.PersistentFailOnCreate.Store("mock create error")
		})

		It("should not update the ServiceExport status to Exported until the sync is successful", func() {
			t.createService()
			t.createServiceExport()

			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
				corev1.ConditionTrue, "AwaitingSync"))

			t.awaitNotServiceExportStatus(&lighthousev2a1.ServiceExportCondition{
				Type:   lighthousev2a1.ServiceExportExported,
				Status: corev1.ConditionTrue,
			})

			t.brokerServiceImportClient.PersistentFailOnCreate.Store("")
			t.awaitServiceExported(t.service.Spec.ClusterIP, 0)
		})
	})

	When("the Service's pod endpoint IPs are lost and then reestablished", func() {
		It("should clear and restore the ServiceImport's IPs", func() {
			t.createService()
			t.createEndpoints()
			t.createServiceExport()
			t.awaitServiceExported(t.service.Spec.ClusterIP, 0)

			t.endpoints.Subsets[0].Addresses = nil
			t.updateEndpoints()
			t.awaitUpdatedServiceImport(t.localServiceImportClient)

			t.endpoints.Subsets[0].Addresses = append(t.endpoints.Subsets[0].Addresses, corev1.EndpointAddress{IP: "192.168.5.10"})
			t.updateEndpoints()
			t.awaitUpdatedServiceImport(t.localServiceImportClient, t.service.Spec.ClusterIP)
		})
	})

	When("the ServiceExportCondition list count reaches MaxExportStatusConditions", func() {
		var oldMaxExportStatusConditions int

		BeforeEach(func() {
			oldMaxExportStatusConditions = controller.MaxExportStatusConditions
			controller.MaxExportStatusConditions = 1
		})

		AfterEach(func() {
			controller.MaxExportStatusConditions = oldMaxExportStatusConditions
		})

		It("should correctly truncate the ServiceExportCondition list", func() {
			t.createService()
			t.createServiceExport()

			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportExported,
				corev1.ConditionTrue, ""))
		})
	})

	When("a ServiceExport is created for a Service whose type is other than ServiceTypeClusterIP", func() {
		BeforeEach(func() {
			t.service.Spec.Type = corev1.ServiceTypeNodePort
		})

		It("should update the ServiceExport status and not sync a ServiceImport", func() {
			t.createService()
			t.createServiceExport()

			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
				corev1.ConditionFalse, "UnsupportedServiceType"))
			t.awaitNoServiceImport(t.brokerServiceImportClient)
		})
	})
})

var _ = Describe("Globalnet enabled", func() {
	t := newTestDiver()

	globalIP := "192.168.10.34"
	t.agentSpec.GlobalnetEnabled = true

	JustBeforeEach(func() {
		t.createService()
		t.createServiceExport()
	})

	When("a local ServiceExport is created and the Service has a global IP", func() {
		BeforeEach(func() {
			t.service.SetAnnotations(map[string]string{"submariner.io/globalIp": globalIP})
		})

		It("should sync a ServiceImport with the global IP of the Service", func() {
			t.awaitServiceExported(globalIP, 0)
		})
	})

	When("a local ServiceExport is created and the Service does not initially have a global IP", func() {
		It("should eventually sync a ServiceImport with the global IP of the Service", func() {
			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
				corev1.ConditionFalse, "ServiceGlobalIPUnavailable"))

			t.service.SetAnnotations(map[string]string{"submariner.io/globalIp": globalIP})
			_, err := t.localServiceClient.CoreV1().Services(t.service.Namespace).Update(t.service)
			Expect(err).To(Succeed())

			t.awaitServiceExported(globalIP, 1)
		})
	})

	When("a ServiceExport is created for a headless Service", func() {
		BeforeEach(func() {
			t.service.Spec.ClusterIP = corev1.ClusterIPNone
		})

		It("should update the ServiceExport status and not sync a ServiceImport", func() {
			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
				corev1.ConditionFalse, "UnsupportedServiceType"))

			t.awaitNoServiceImport(t.brokerServiceImportClient)
		})
	})
})

var _ = Describe("Headless service syncing", func() {
	t := newTestDiver()

	BeforeEach(func() {
		t.service.Spec.ClusterIP = corev1.ClusterIPNone
	})

	JustBeforeEach(func() {
		t.createService()
	})

	When("a ServiceExport is created", func() {
		When("the Endpoints already exists", func() {
			It("should correctly sync a ServiceImport", func() {
				t.createEndpoints()
				t.createServiceExport()

				t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)
				t.awaitServiceImport(t.brokerServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)
			})
		})

		When("the Endpoints doesn't initially exist", func() {
			It("should eventually sync a correct ServiceImport", func() {
				t.createServiceExport()

				t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.Headless)
				t.awaitServiceImport(t.brokerServiceImportClient, lighthousev2a1.Headless)

				t.createEndpoints()

				t.awaitUpdatedServiceImport(t.localServiceImportClient, t.endpointIPs()...)
				t.awaitUpdatedServiceImport(t.brokerServiceImportClient, t.endpointIPs()...)
			})
		})
	})

	When("the Endpoints for a service are updated", func() {
		It("should update the ServiceImport", func() {
			t.createEndpoints()
			t.createServiceExport()

			t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)
			t.awaitServiceImport(t.brokerServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)

			t.endpoints.Subsets[0].Addresses = append(t.endpoints.Subsets[0].Addresses, corev1.EndpointAddress{IP: "192.168.5.3"})
			t.updateEndpoints()

			t.awaitUpdatedServiceImport(t.localServiceImportClient, t.endpointIPs()...)
			t.awaitUpdatedServiceImport(t.brokerServiceImportClient, t.endpointIPs()...)
		})
	})
})

var _ = Describe("Service export failures", func() {
	t := newTestDiver()

	JustBeforeEach(func() {
		t.createService()
		t.createEndpoints()
		t.createServiceExport()
	})

	When("Service retrieval initially fails", func() {
		BeforeEach(func() {
			t.servicesReactor.SetFailOnGet(errors.New("fake Get error"))
		})

		It("should update the ServiceExport status and eventually sync a ServiceImport", func() {
			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
				corev1.ConditionUnknown, "ServiceRetrievalFailed"))
			t.servicesReactor.SetResetOnFailure(true)
			t.awaitServiceExported(t.service.Spec.ClusterIP, 1)
		})
	})

	When("Endpoints retrieval initially fails", func() {
		BeforeEach(func() {
			t.service.Spec.ClusterIP = corev1.ClusterIPNone
			t.endpointsReactor.SetFailOnGet(errors.New("fake Get error"))
		})

		It("should update the ServiceExport status and eventually sync a ServiceImport", func() {
			t.awaitServiceExportStatus(0, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
				corev1.ConditionUnknown, "ServiceRetrievalFailed"))
			t.endpointsReactor.SetResetOnFailure(true)
			t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)
		})
	})

	When("an exported Service is deleted and ServiceExport retrieval initially fails", func() {
		It("should eventually delete the ServiceImport", func() {
			t.awaitServiceExported(t.service.Spec.ClusterIP, 0)

			t.serviceExportReactor.SetFailOnGet(errors.New("fake Get error"))
			t.serviceExportReactor.SetResetOnFailure(true)
			t.deleteService()
			t.awaitNoServiceImport(t.localServiceImportClient)
		})
	})

	When("Endpoints is updated and ServiceExport retrieval initially fails", func() {
		BeforeEach(func() {
			t.service.Spec.ClusterIP = corev1.ClusterIPNone
		})

		It("should eventually update the ServiceImport", func() {
			t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)

			t.serviceExportReactor.SetFailOnGet(errors.New("fake Get error"))
			t.serviceExportReactor.SetResetOnFailure(true)
			t.endpoints.Subsets[0].Addresses = append(t.endpoints.Subsets[0].Addresses, corev1.EndpointAddress{IP: "192.168.5.3"})
			t.updateEndpoints()

			t.awaitUpdatedServiceImport(t.localServiceImportClient, t.endpointIPs()...)
		})
	})

	When("Endpoints is updated and ServiceImport retrieval initially fails", func() {
		BeforeEach(func() {
			t.service.Spec.ClusterIP = corev1.ClusterIPNone
		})

		It("should eventually update the ServiceImport", func() {
			t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.Headless, t.endpointIPs()...)

			t.serviceImportReactor.SetFailOnGet(errors.New("fake Get error"))
			t.serviceImportReactor.SetResetOnFailure(true)
			t.endpoints.Subsets[0].Addresses = append(t.endpoints.Subsets[0].Addresses, corev1.EndpointAddress{IP: "192.168.5.3"})
			t.updateEndpoints()

			t.awaitUpdatedServiceImport(t.localServiceImportClient, t.endpointIPs()...)
		})
	})

	When("a conflict initially occurs when updating the ServiceExport status", func() {
		BeforeEach(func() {
			t.serviceExportReactor.SetFailOnUpdate(apierrors.NewConflict(schema.GroupResource{}, t.serviceExport.Name,
				errors.New("fake conflict")))
			t.serviceExportReactor.SetResetOnFailure(true)
		})

		It("should eventually update the ServiceExport status", func() {
			t.awaitServiceExported(t.service.Spec.ClusterIP, 0)
		})
	})
})

type testDriver struct {
	agentController           *controller.Controller
	agentSpec                 *controller.AgentSpecification
	localDynClient            dynamic.Interface
	brokerDynClient           dynamic.Interface
	localServiceExportClient  dynamic.ResourceInterface
	localServiceImportClient  dynamic.ResourceInterface
	brokerServiceImportClient *fake.DynamicResourceClient
	localServiceClient        kubernetes.Interface
	lighthouseClient          lighthouseClientset.Interface
	service                   *corev1.Service
	serviceExport             *lighthousev2a1.ServiceExport
	endpoints                 *corev1.Endpoints
	stopCh                    chan struct{}
	now                       time.Time
	restMapper                meta.RESTMapper
	endpointsReactor          *FailingReactor
	servicesReactor           *FailingReactor
	serviceExportReactor      *FailingReactor
	serviceImportReactor      *FailingReactor
}

func newTestDiver() *testDriver {
	t := &testDriver{agentSpec: &controller.AgentSpecification{
		ClusterID:        clusterID,
		Namespace:        test.LocalNamespace,
		GlobalnetEnabled: false,
	}}

	BeforeEach(func() {
		t.now = time.Now()
		t.stopCh = make(chan struct{})

		t.service = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nginx",
				Namespace: test.LocalNamespace,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "10.253.9.1",
			},
		}

		t.serviceExport = &lighthousev2a1.ServiceExport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      t.service.Name,
				Namespace: t.service.Namespace,
			},
		}

		t.endpoints = &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:      t.service.Name,
				Namespace: t.service.Namespace,
			},
			Subsets: []corev1.EndpointSubset{
				{
					Addresses: []corev1.EndpointAddress{
						{
							IP: "192.168.5.1",
						},
						{
							IP: "192.168.5.2",
						},
					},
				},
			},
		}

		t.restMapper = test.GetRESTMapperFor(&lighthousev2a1.ServiceExport{}, &lighthousev2a1.ServiceImport{},
			&corev1.Service{}, &corev1.Endpoints{})

		t.localDynClient = fake.NewDynamicClient()
		t.brokerDynClient = fake.NewDynamicClient()

		t.localServiceExportClient = t.localDynClient.Resource(*test.GetGroupVersionResourceFor(t.restMapper,
			&lighthousev2a1.ServiceExport{})).Namespace(test.LocalNamespace)

		t.localServiceImportClient = t.localDynClient.Resource(*test.GetGroupVersionResourceFor(t.restMapper,
			&lighthousev2a1.ServiceImport{})).Namespace(test.LocalNamespace)

		t.brokerServiceImportClient = t.brokerDynClient.Resource(*test.GetGroupVersionResourceFor(t.restMapper,
			&lighthousev2a1.ServiceImport{})).Namespace(test.RemoteNamespace).(*fake.DynamicResourceClient)

		fakeCS := fakeKubeClient.NewSimpleClientset()
		t.endpointsReactor = NewFailingReactorForResource(&fakeCS.Fake, "endpoints")
		t.servicesReactor = NewFailingReactorForResource(&fakeCS.Fake, "services")
		t.localServiceClient = fakeCS

		fakeLH := fakeLighthouseClientset.NewSimpleClientset()
		t.serviceExportReactor = NewFailingReactorForResource(&fakeLH.Fake, "serviceexports")
		t.serviceImportReactor = NewFailingReactorForResource(&fakeLH.Fake, "serviceimports")
		t.lighthouseClient = fakeLH
	})

	JustBeforeEach(func() {
		syncerConfig := &broker.SyncerConfig{
			BrokerNamespace: test.RemoteNamespace,
		}

		syncerScheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(syncerScheme)).To(Succeed())
		Expect(lighthousev2a1.AddToScheme(syncerScheme)).To(Succeed())

		var err error
		t.agentController, err = controller.NewWithDetail(t.agentSpec, syncerConfig, t.restMapper, t.localDynClient,
			t.localServiceClient, t.lighthouseClient, syncerScheme, func(config *broker.SyncerConfig) (*broker.Syncer, error) {
				return broker.NewSyncerWithDetail(config, t.localDynClient, t.brokerDynClient, t.restMapper)
			})

		Expect(err).To(Succeed())

		Expect(t.agentController.Start(t.stopCh)).To(Succeed())
	})

	AfterEach(func() {
		close(t.stopCh)
	})

	return t
}

func (t *testDriver) createService() {
	_, err := t.localServiceClient.CoreV1().Services(t.service.Namespace).Create(t.service)
	Expect(err).To(Succeed())

	test.CreateResource(t.dynamicServiceClient(), t.service)
}

func (t *testDriver) createEndpoints() {
	_, err := t.localServiceClient.CoreV1().Endpoints(t.endpoints.Namespace).Create(t.endpoints)
	Expect(err).To(Succeed())

	test.CreateResource(t.dynamicEndpointsClient(), t.endpoints)
}

func (t *testDriver) updateEndpoints() {
	_, err := t.localServiceClient.CoreV1().Endpoints(t.endpoints.Namespace).Update(t.endpoints)
	Expect(err).To(Succeed())

	test.UpdateResource(t.dynamicEndpointsClient(), t.endpoints)
}

func (t *testDriver) dynamicEndpointsClient() dynamic.ResourceInterface {
	return t.localDynClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "endpoints"}).Namespace(t.service.Namespace)
}

func (t *testDriver) createServiceExport() {
	_, err := t.lighthouseClient.LighthouseV2alpha1().ServiceExports(t.serviceExport.Namespace).Create(t.serviceExport)
	Expect(err).To(Succeed())

	test.CreateResource(t.localServiceExportClient, t.serviceExport)
}

func (t *testDriver) deleteServiceExport() {
	Expect(t.localServiceExportClient.Delete(t.service.GetName(), nil)).To(Succeed())
}

func (t *testDriver) deleteService() {
	Expect(t.dynamicServiceClient().Delete(t.service.Name, nil)).To(Succeed())

	Expect(t.localServiceClient.CoreV1().Services(t.service.Namespace).Delete(t.service.Name, nil)).To(Succeed())
}

func (t *testDriver) dynamicServiceClient() dynamic.ResourceInterface {
	return t.localDynClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "services"}).Namespace(t.service.Namespace)
}

func (t *testDriver) awaitServiceImport(client dynamic.ResourceInterface, sType lighthousev2a1.ServiceImportType, serviceIPs ...string) {
	obj := test.AwaitResource(client, t.service.Name+"-"+t.service.Namespace+"-"+clusterID)

	serviceImport := &lighthousev2a1.ServiceImport{}
	Expect(scheme.Scheme.Convert(obj, serviceImport, nil)).To(Succeed())

	Expect(serviceImport.GetAnnotations()["origin-name"]).To(Equal(t.service.Name))
	Expect(serviceImport.GetAnnotations()["origin-namespace"]).To(Equal(t.service.Namespace))
	Expect(serviceImport.Spec.Type).To(Equal(sType))

	Expect(serviceImport.Status.Clusters).To(HaveLen(1))
	Expect(serviceImport.Status.Clusters[0].Cluster).To(Equal(clusterID))

	sort.Strings(serviceIPs)
	sort.Strings(serviceImport.Status.Clusters[0].IPs)
	Expect(serviceImport.Status.Clusters[0].IPs).To(Equal(serviceIPs))

	_, err := t.lighthouseClient.LighthouseV2alpha1().ServiceImports(serviceImport.Namespace).Create(serviceImport)
	Expect(err).To(Succeed())
}

func (t *testDriver) awaitUpdatedServiceImport(client dynamic.ResourceInterface, serviceIPs ...string) {
	name := t.service.Name + "-" + t.service.Namespace + "-" + clusterID

	sort.Strings(serviceIPs)

	var serviceImport *lighthousev2a1.ServiceImport

	err := wait.PollImmediate(50*time.Millisecond, 5*time.Second, func() (bool, error) {
		obj, err := client.Get(name, metav1.GetOptions{})
		Expect(err).To(Succeed())

		serviceImport = &lighthousev2a1.ServiceImport{}
		Expect(scheme.Scheme.Convert(obj, serviceImport, nil)).To(Succeed())

		sort.Strings(serviceImport.Status.Clusters[0].IPs)

		return reflect.DeepEqual(serviceImport.Status.Clusters[0].IPs, serviceIPs), nil
	})

	if err == wait.ErrWaitTimeout {
		Expect(serviceImport.Status.Clusters[0].IPs).To(Equal(serviceIPs))
	}

	Expect(err).To(Succeed())

	_, err = t.lighthouseClient.LighthouseV2alpha1().ServiceImports(serviceImport.Namespace).Update(serviceImport)
	Expect(err).To(Succeed())
}

func (t *testDriver) awaitNoServiceImport(client dynamic.ResourceInterface) {
	test.AwaitNoResource(client, t.service.Name+"-"+t.service.Namespace+"-"+clusterID)
}

func (t *testDriver) awaitServiceExportStatus(atIndex int, expCond ...*lighthousev2a1.ServiceExportCondition) {
	var found *lighthousev2a1.ServiceExport

	err := wait.PollImmediate(50*time.Millisecond, 5*time.Second, func() (bool, error) {
		se, err := t.lighthouseClient.LighthouseV2alpha1().ServiceExports(t.service.Namespace).Get(t.service.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}

		found = se

		if (atIndex + len(expCond)) != len(se.Status.Conditions) {
			return false, nil
		}

		j := atIndex
		for _, exp := range expCond {
			actual := se.Status.Conditions[j]
			j++
			Expect(actual.Type).To(Equal(exp.Type))
			Expect(actual.Status).To(Equal(exp.Status))
			Expect(actual.LastTransitionTime).To(Not(BeNil()))
			Expect(actual.LastTransitionTime.After(t.now)).To(BeTrue())
			Expect(actual.Reason).To(Not(BeNil()))
			Expect(*actual.Reason).To(Equal(*exp.Reason))
			Expect(actual.Message).To(Not(BeNil()))
		}

		return true, nil
	})

	if err == wait.ErrWaitTimeout {
		if found == nil {
			Fail("ServiceExport not found")
		}

		Fail(format.Message(found.Status.Conditions, fmt.Sprintf("to contain at index %d", atIndex), expCond))
	} else {
		Expect(err).To(Succeed())
	}
}

func (t *testDriver) awaitNotServiceExportStatus(notCond *lighthousev2a1.ServiceExportCondition) {
	err := wait.PollImmediate(50*time.Millisecond, 300*time.Millisecond, func() (bool, error) {
		se, err := t.lighthouseClient.LighthouseV2alpha1().ServiceExports(t.service.Namespace).Get(t.service.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		if err != nil {
			return false, err
		}

		if len(se.Status.Conditions) == 0 {
			return false, nil
		}

		last := &se.Status.Conditions[len(se.Status.Conditions)-1]
		if last.Type == notCond.Type && last.Status == notCond.Status {
			return false, fmt.Errorf("Received unexpected %#v", last)
		}

		return false, nil
	})

	if err != wait.ErrWaitTimeout {
		Fail(err.Error())
	}
}

func (t *testDriver) awaitServiceExported(serviceIP string, statusIndex int) int {
	t.awaitServiceImport(t.brokerServiceImportClient, lighthousev2a1.ClusterSetIP, serviceIP)
	t.awaitServiceImport(t.localServiceImportClient, lighthousev2a1.ClusterSetIP, serviceIP)

	t.awaitServiceExportStatus(statusIndex, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
		corev1.ConditionTrue, "AwaitingSync"), newServiceExportCondition(lighthousev2a1.ServiceExportExported,
		corev1.ConditionTrue, ""))

	return statusIndex + 2
}

func (t *testDriver) awaitServiceUnavailableStatus(atIndex int) {
	t.awaitServiceExportStatus(atIndex, newServiceExportCondition(lighthousev2a1.ServiceExportInitialized,
		corev1.ConditionFalse, "ServiceUnavailable"))
}

func (t *testDriver) endpointIPs() []string {
	ips := []string{}
	for _, a := range t.endpoints.Subsets[0].Addresses {
		ips = append(ips, a.IP)
	}

	return ips
}

func newServiceExportCondition(cType lighthousev2a1.ServiceExportConditionType,
	status corev1.ConditionStatus, reason string) *lighthousev2a1.ServiceExportCondition {
	return &lighthousev2a1.ServiceExportCondition{
		Type:   cType,
		Status: status,
		Reason: &reason,
	}
}

type FailingReactor struct {
	sync.Mutex
	failOnCreate   error
	failOnUpdate   error
	failOnDelete   error
	failOnGet      error
	failOnList     error
	resetOnFailure bool
}

func NewFailingReactorForResource(f *testing.Fake, resource string) *FailingReactor {
	r := &FailingReactor{}
	chain := []testing.Reactor{&testing.SimpleReactor{Verb: "*", Resource: resource, Reaction: r.react}}
	f.ReactionChain = append(chain, f.ReactionChain...)

	return r
}

func (f *FailingReactor) react(action testing.Action) (bool, runtime.Object, error) {
	f.Lock()
	defer f.Unlock()

	switch action.GetVerb() {
	case "get":
		return f.get()
	case "create":
		return f.create()
	case "update":
		return f.update()
	case "delete":
		return f.delete()
	case "list":
		return f.list()
	}

	return false, nil, nil
}

func (f *FailingReactor) get() (bool, runtime.Object, error) {
	err := f.failOnGet
	if err != nil {
		if f.resetOnFailure {
			f.failOnGet = nil
		}

		return true, nil, err
	}

	return false, nil, nil
}

func (f *FailingReactor) create() (bool, runtime.Object, error) {
	err := f.failOnCreate
	if err != nil {
		if f.resetOnFailure {
			f.failOnCreate = nil
		}

		return true, nil, err
	}

	return false, nil, nil
}

func (f *FailingReactor) update() (bool, runtime.Object, error) {
	err := f.failOnUpdate
	if err != nil {
		if f.resetOnFailure {
			f.failOnUpdate = nil
		}

		return true, nil, err
	}

	return false, nil, nil
}

func (f *FailingReactor) delete() (bool, runtime.Object, error) {
	err := f.failOnDelete
	if err != nil {
		if f.resetOnFailure {
			f.failOnDelete = nil
		}

		return true, nil, err
	}

	return false, nil, nil
}

func (f *FailingReactor) list() (bool, runtime.Object, error) {
	err := f.failOnList
	if err != nil {
		if f.resetOnFailure {
			f.failOnList = nil
		}

		return true, nil, err
	}

	return false, nil, nil
}

func (f *FailingReactor) SetResetOnFailure(v bool) {
	f.Lock()
	defer f.Unlock()
	f.resetOnFailure = v
}

func (f *FailingReactor) SetFailOnCreate(err error) {
	f.Lock()
	defer f.Unlock()
	f.failOnCreate = err
}

func (f *FailingReactor) SetFailOnUpdate(err error) {
	f.Lock()
	defer f.Unlock()
	f.failOnUpdate = err
}

func (f *FailingReactor) SetFailOnDelete(err error) {
	f.Lock()
	defer f.Unlock()
	f.failOnDelete = err
}

func (f *FailingReactor) SetFailOnGet(err error) {
	f.Lock()
	defer f.Unlock()
	f.failOnGet = err
}

func (f *FailingReactor) SetFailOnList(err error) {
	f.Lock()
	defer f.Unlock()
	f.failOnList = err
}