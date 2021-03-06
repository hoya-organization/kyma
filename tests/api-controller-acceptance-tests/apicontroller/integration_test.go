package apicontroller

import (
	"fmt"
	"github.com/kyma-project/kyma/common/ingressgateway"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	kymaApi "github.com/kyma-project/kyma/components/api-controller/pkg/apis/gateway.kyma-project.io/v1alpha2"
	kyma "github.com/kyma-project/kyma/components/api-controller/pkg/clients/gateway.kyma-project.io/clientset/versioned"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type integrationTestContext struct{}

func TestIntegrationSpec(t *testing.T) {

	domainName := os.Getenv(domainNameEnv)
	if domainName == "" {
		t.Fatal("Domain name not set.")
	}

	namespace := os.Getenv(namespaceEnv)
	if namespace == "" {
		t.Fatal("Namespace not set.")
	}

	ctx := integrationTestContext{}
	testID := ctx.generateTestID(testIDLength)

	log.Infof("Running test: %s", testID)

	httpClient, err := ingressgateway.FromEnv().Client()
	if err != nil {
		t.Fatalf("Cannot get ingressgateway client: %s", err)
	}

	kubeConfig := ctx.defaultConfigOrExit()
	k8sInterface := ctx.k8sInterfaceOrExit(kubeConfig)

	t.Logf("Set up...")
	fixture := setUpOrExit(k8sInterface, namespace, testID)

	var lastAPI *kymaApi.Api

	suiteFinished := false

	Convey("API Controller should", t, func() {

		Reset(func() {
			if suiteFinished {
				t.Logf("Tear down...")
				fixture.tearDown()
			}
		})

		kymaInterface, kymaErr := kyma.NewForConfig(kubeConfig)
		if kymaErr != nil {
			log.Fatalf("can create kyma clientset. Root cause: %v", kymaErr)
		}

		suiteFinished = false

		Convey("create API with authentication disabled", func() {

			api := ctx.apiFor(testID, domainName, namespace, fixture.SampleAppService, apiSecurityDisabled, true)

			lastAPI, err = kymaInterface.GatewayV1alpha2().Apis(namespace).Create(api)
			So(err, ShouldBeNil)
			So(lastAPI, ShouldNotBeNil)
			So(lastAPI.ResourceVersion, ShouldNotBeEmpty)

			ctx.validateAPINotSecured(httpClient, lastAPI.Spec.Hostname)
			lastAPI, err = kymaInterface.GatewayV1alpha2().Apis(namespace).Get(lastAPI.Name, metav1.GetOptions{})
			So(err, ShouldBeNil)
			So(lastAPI, ShouldNotBeNil)
			So(lastAPI.ResourceVersion, ShouldNotBeEmpty)
		})

		Convey("update API with custom jwt configuration", func() {

			api := *lastAPI
			ctx.setCustomJwtAuthenticationConfig(&api)

			lastAPI, err = kymaInterface.GatewayV1alpha2().Apis(namespace).Update(&api)
			So(err, ShouldBeNil)
			So(lastAPI, ShouldNotBeNil)
			So(lastAPI.ResourceVersion, ShouldNotBeEmpty)

			ctx.validateAPISecured(httpClient, lastAPI)
			lastAPI, err = kymaInterface.GatewayV1alpha2().Apis(namespace).Get(lastAPI.Name, metav1.GetOptions{})
			So(err, ShouldBeNil)
			So(lastAPI, ShouldNotBeNil)
			So(lastAPI.ResourceVersion, ShouldNotBeEmpty)
		})

		Convey("delete API", func() {

			suiteFinished = true
			ctx.checkPreconditions(lastAPI, t)

			err := kymaInterface.GatewayV1alpha2().Apis(namespace).Delete(lastAPI.Name, &metav1.DeleteOptions{})
			So(err, ShouldBeNil)

			_, err = kymaInterface.GatewayV1alpha2().Apis(namespace).Get(lastAPI.Name, metav1.GetOptions{})
			So(err, ShouldNotBeNil)
		})
	})
}

func (ctx integrationTestContext) apiFor(testID string, domainName string, namespace string, svc *apiv1.Service, secured APISecurity, hostWithDomain bool) *kymaApi.Api {

	return &kymaApi.Api{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      fmt.Sprintf("sample-app-api-%s", testID),
		},
		Spec: kymaApi.ApiSpec{
			Hostname: ctx.hostnameFor(testID, domainName, hostWithDomain),
			Service: kymaApi.Service{
				Name: svc.Name,
				Port: int(svc.Spec.Ports[0].Port),
			},
			AuthenticationEnabled: (*bool)(&secured),
			Authentication:        []kymaApi.AuthenticationRule{},
		},
	}
}

func (integrationTestContext) setCustomJwtAuthenticationConfig(api *kymaApi.Api) {
	// OTHER EXAMPLE OF POSSIBLE VALUES:
	//issuer := "https://accounts.google.com"
	//jwksURI := "https://www.googleapis.com/oauth2/v3/certs"

	issuer := "https://accounts.google.com"
	jwksURI := "http://dex-service.kyma-system.svc.cluster.local:5556/keys"

	rules := []kymaApi.AuthenticationRule{
		{
			Type: kymaApi.JwtType,
			Jwt: kymaApi.JwtAuthentication{
				Issuer:  issuer,
				JwksUri: jwksURI,
			},
		},
	}

	secured := true
	if api.Spec.AuthenticationEnabled != nil && !(*api.Spec.AuthenticationEnabled) { // optional property, but if set earlier to false it will force auth disabled
		api.Spec.AuthenticationEnabled = &secured
	}
	api.Spec.Authentication = rules
}

func (integrationTestContext) checkPreconditions(lastAPI *kymaApi.Api, t *testing.T) {
	if lastAPI == nil {
		t.Fatal("Precondition failed - last API not set")
	}
}

func (integrationTestContext) hostnameFor(testID, domainName string, hostWithDomain bool) string {
	if hostWithDomain {
		return fmt.Sprintf("%s.%s", testID, domainName)
	}
	return testID
}

func (ctx integrationTestContext) validateAPISecured(httpClient *http.Client, api *kymaApi.Api) {

	response, err := ctx.withRetries(maxRetries, minimalNumberOfCorrectResults, func() (*http.Response, error) {
		return httpClient.Get(fmt.Sprintf("https://%s", api.Spec.Hostname))
	}, ctx.httpUnauthorizedPredicate)

	So(err, ShouldBeNil)
	So(response.StatusCode, ShouldEqual, http.StatusUnauthorized)
}

func (ctx integrationTestContext) validateAPINotSecured(httpClient *http.Client, hostname string) {

	response, err := ctx.withRetries(maxRetries, minimalNumberOfCorrectResults, func() (*http.Response, error) {
		return httpClient.Get(fmt.Sprintf("https://%s", hostname))
	}, ctx.httpOkPredicate)

	So(err, ShouldBeNil)
	So(response.StatusCode, ShouldEqual, http.StatusOK)
}

func (integrationTestContext) withRetries(maxRetries, minCorrect int, httpCall func() (*http.Response, error), shouldRetryPredicate func(*http.Response) bool) (*http.Response, error) {

	var response *http.Response
	var err error

	count := 0
	retry := true
	for retryNo := 0; retry; retryNo++ {

		log.Debugf("[%d / %d] Retrying...", retryNo, maxRetries)
		response, err = httpCall()

		if err != nil {
			log.Errorf("[%d / %d] Got error: %s", retryNo, maxRetries, err)
			count = 0
		} else if shouldRetryPredicate(response) {
			log.Errorf("[%d / %d] Got response: %s", retryNo, maxRetries, response.Status)
			count = 0
		} else {
			log.Infof("Got expected response %d in a row.", count+1)
			if count++; count == minCorrect {
				log.Infof("Reached minimal number of expected responses in a row. Do not need to retry anymore.")
				retry = false
			}
		}

		if retry {

			if retryNo >= maxRetries {
				// do not retry anymore
				log.Infof("No more retries (max retries exceeded).")
				retry = false
			} else {
				time.Sleep(retrySleep)
			}
		}
	}

	return response, err
}

func (integrationTestContext) httpOkPredicate(response *http.Response) bool {
	return response.StatusCode < 200 || response.StatusCode > 299
}

func (integrationTestContext) httpUnauthorizedPredicate(response *http.Response) bool {
	return response.StatusCode != 401
}

func (integrationTestContext) defaultConfigOrExit() *rest.Config {

	kubeConfigLocation := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	kubeConfig, err := clientcmd.BuildConfigFromFlags("", kubeConfigLocation)
	if err != nil {
		log.Debugf("unable to load local kube config. Root cause: %v", err)
		if config, err2 := rest.InClusterConfig(); err2 != nil {
			log.Fatalf("unable to load kube config. Root cause: %v", err2)
		} else {
			kubeConfig = config
		}
	}
	return kubeConfig
}

func (integrationTestContext) k8sInterfaceOrExit(kubeConfig *rest.Config) kubernetes.Interface {

	k8sInterface, k8sErr := kubernetes.NewForConfig(kubeConfig)
	if k8sErr != nil {
		log.Fatalf("can create k8s clientset. Root cause: %v", k8sErr)
	}
	return k8sInterface
}

func (integrationTestContext) generateTestID(n int) string {

	rand.Seed(time.Now().UnixNano())

	letterRunes := []rune("abcdefghijklmnopqrstuvwxyz")

	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}
