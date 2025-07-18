package extension_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/argoproj/argo-cd/v3/util/rbac"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/server/extension"
	"github.com/argoproj/argo-cd/v3/server/extension/mocks"
	dbmocks "github.com/argoproj/argo-cd/v3/util/db/mocks"
	"github.com/argoproj/argo-cd/v3/util/settings"
)

func TestValidateHeaders(t *testing.T) {
	t.Run("will build RequestResources successfully", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "namespace:app-name")
		r.Header.Add(extension.HeaderArgoCDProjectName, "project-name")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.NoError(t, err)
		assert.NotNil(t, rr)
		assert.Equal(t, "namespace", rr.ApplicationNamespace)
		assert.Equal(t, "app-name", rr.ApplicationName)
		assert.Equal(t, "project-name", rr.ProjectName)
	})
	t.Run("will return error if application is malformatted", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "no-namespace")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.Error(t, err)
		assert.Nil(t, rr)
	})
	t.Run("will return error if application header is missing", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDProjectName, "project-name")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.Error(t, err)
		assert.Nil(t, rr)
	})
	t.Run("will return error if project header is missing", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "namespace:app-name")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.Error(t, err)
		assert.Nil(t, rr)
	})
	t.Run("will return error if invalid namespace", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "bad%namespace:app-name")
		r.Header.Add(extension.HeaderArgoCDProjectName, "project-name")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.Error(t, err)
		assert.Nil(t, rr)
	})
	t.Run("will return error if invalid app name", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "namespace:bad@app")
		r.Header.Add(extension.HeaderArgoCDProjectName, "project-name")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.Error(t, err)
		assert.Nil(t, rr)
	})
	t.Run("will return error if invalid project name", func(t *testing.T) {
		// given
		r, err := http.NewRequest(http.MethodGet, "http://null", http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "namespace:app")
		r.Header.Add(extension.HeaderArgoCDProjectName, "bad^project")

		// when
		rr, err := extension.ValidateHeaders(r)

		// then
		require.Error(t, err)
		assert.Nil(t, rr)
	})
}

func TestRegisterExtensions(t *testing.T) {
	t.Parallel()

	type fixture struct {
		settingsGetterMock *mocks.SettingsGetter
		manager            *extension.Manager
	}

	setup := func() *fixture {
		settMock := &mocks.SettingsGetter{}

		logger, _ := test.NewNullLogger()
		logEntry := logger.WithContext(t.Context())
		m := extension.NewManager(logEntry, "", settMock, nil, nil, nil, nil, nil)

		return &fixture{
			settingsGetterMock: settMock,
			manager:            m,
		}
	}
	t.Run("will register extensions successfully", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		settings := &settings.ArgoCDSettings{
			ExtensionConfig: map[string]string{
				"":            getExtensionConfigString(),
				"another-ext": getSingleExtensionConfigString(),
			},
		}
		f.settingsGetterMock.On("Get", mock.Anything).Return(settings, nil)
		expectedProxyRegistries := []string{
			"external-backend",
			"some-backend",
			"another-ext",
		}

		// when
		err := f.manager.RegisterExtensions()

		// then
		require.NoError(t, err)
		for _, expectedProxyRegistry := range expectedProxyRegistries {
			proxyRegistry, found := f.manager.ProxyRegistry(expectedProxyRegistry)
			assert.True(t, found)
			assert.NotNil(t, proxyRegistry)
		}
	})
	t.Run("will return error if extension config is invalid", func(t *testing.T) {
		// given
		t.Parallel()
		type testCase struct {
			name       string
			configYaml string
		}
		cases := []testCase{
			{
				name:       "no name",
				configYaml: getExtensionConfigNoName(),
			},
			{
				name:       "no service",
				configYaml: getExtensionConfigNoService(),
			},
			{
				name:       "no URL",
				configYaml: getExtensionConfigNoURL(),
			},
			{
				name:       "invalid name",
				configYaml: getExtensionConfigInvalidName(),
			},
			{
				name:       "no header name",
				configYaml: getExtensionConfigNoHeaderName(),
			},
			{
				name:       "no header value",
				configYaml: getExtensionConfigNoHeaderValue(),
			},
		}

		// when
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				// given
				t.Parallel()
				f := setup()
				settings := &settings.ArgoCDSettings{
					ExtensionConfig: map[string]string{
						"": tc.configYaml,
					},
				}
				f.settingsGetterMock.On("Get", mock.Anything).Return(settings, nil)

				// when
				err := f.manager.RegisterExtensions()

				// then
				require.Error(t, err, "expected error in test %s but got nil", tc.name)
			})
		}
	})
}

func TestCallExtension(t *testing.T) {
	t.Parallel()

	type fixture struct {
		mux                *http.ServeMux
		appGetterMock      *mocks.ApplicationGetter
		settingsGetterMock *mocks.SettingsGetter
		rbacMock           *mocks.RbacEnforcer
		projMock           *mocks.ProjectGetter
		metricsMock        *mocks.ExtensionMetricsRegistry
		userMock           *mocks.UserGetter
		manager            *extension.Manager
	}
	defaultServerNamespace := "control-plane-ns"
	defaultProjectName := "project-name"

	setup := func() *fixture {
		appMock := &mocks.ApplicationGetter{}
		settMock := &mocks.SettingsGetter{}
		rbacMock := &mocks.RbacEnforcer{}
		projMock := &mocks.ProjectGetter{}
		metricsMock := &mocks.ExtensionMetricsRegistry{}
		userMock := &mocks.UserGetter{}

		dbMock := &dbmocks.ArgoDB{}
		dbMock.On("GetClusterServersByName", mock.Anything, mock.Anything).Return([]string{"cluster1"}, nil)
		dbMock.On("GetCluster", mock.Anything, mock.Anything).Return(&v1alpha1.Cluster{Server: "some-url", Name: "cluster1"}, nil)

		logger, _ := test.NewNullLogger()
		logEntry := logger.WithContext(t.Context())
		m := extension.NewManager(logEntry, defaultServerNamespace, settMock, appMock, projMock, dbMock, rbacMock, userMock)
		m.AddMetricsRegistry(metricsMock)

		mux := http.NewServeMux()
		extHandler := http.HandlerFunc(m.CallExtension())
		mux.Handle(extension.URLPrefix+"/", extHandler)

		return &fixture{
			mux:                mux,
			appGetterMock:      appMock,
			settingsGetterMock: settMock,
			rbacMock:           rbacMock,
			projMock:           projMock,
			metricsMock:        metricsMock,
			userMock:           userMock,
			manager:            m,
		}
	}

	getApp := func(destName, destServer, projName string) *v1alpha1.Application {
		return &v1alpha1.Application{
			TypeMeta:   metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{},
			Spec: v1alpha1.ApplicationSpec{
				Destination: v1alpha1.ApplicationDestination{
					Name:   destName,
					Server: destServer,
				},
				Project: projName,
			},
			Status: v1alpha1.ApplicationStatus{
				Resources: []v1alpha1.ResourceStatus{
					{
						Group:     "apps",
						Version:   "v1",
						Kind:      "Pod",
						Namespace: "default",
						Name:      "some-pod",
					},
				},
			},
		}
	}

	getProjectWithDestinations := func(prjName string, destNames []string, destURLs []string) *v1alpha1.AppProject {
		destinations := []v1alpha1.ApplicationDestination{}
		for _, destName := range destNames {
			destination := v1alpha1.ApplicationDestination{
				Name: destName,
			}
			destinations = append(destinations, destination)
		}
		for _, destURL := range destURLs {
			destination := v1alpha1.ApplicationDestination{
				Server: destURL,
			}
			destinations = append(destinations, destination)
		}
		return &v1alpha1.AppProject{
			ObjectMeta: metav1.ObjectMeta{
				Name: prjName,
			},
			Spec: v1alpha1.AppProjectSpec{
				Destinations: destinations,
			},
		}
	}

	withProject := func(prj *v1alpha1.AppProject, f *fixture) {
		f.projMock.On("Get", prj.GetName()).Return(prj, nil)
	}

	withMetrics := func(f *fixture) {
		f.metricsMock.On("IncExtensionRequestCounter", mock.Anything, mock.Anything)
		f.metricsMock.On("ObserveExtensionRequestDuration", mock.Anything, mock.Anything)
	}

	withRbac := func(f *fixture, allowApp, allowExt bool) {
		var appAccessError error
		var extAccessError error
		if !allowApp {
			appAccessError = errors.New("no app permission")
		}
		if !allowExt {
			extAccessError = errors.New("no extension permission")
		}
		f.rbacMock.On("EnforceErr", mock.Anything, rbac.ResourceApplications, rbac.ActionGet, mock.Anything).Return(appAccessError)
		f.rbacMock.On("EnforceErr", mock.Anything, rbac.ResourceExtensions, rbac.ActionInvoke, mock.Anything).Return(extAccessError)
	}

	withUser := func(f *fixture, username string, groups []string) {
		f.userMock.On("GetUser", mock.Anything).Return(username)
		f.userMock.On("GetGroups", mock.Anything).Return(groups)
	}

	withExtensionConfig := func(configYaml string, f *fixture) {
		secrets := make(map[string]string)
		secrets["extension.auth.header"] = "Bearer some-bearer-token"
		secrets["extension.auth.header2"] = "Bearer another-bearer-token"

		settings := &settings.ArgoCDSettings{
			ExtensionConfig: map[string]string{
				"ephemeral": "services:\n- url: http://some-server.com",
				"":          configYaml,
			},
			Secrets: secrets,
		}
		f.settingsGetterMock.On("Get", mock.Anything).Return(settings, nil)
	}

	startTestServer := func(t *testing.T, f *fixture) *httptest.Server {
		t.Helper()
		err := f.manager.RegisterExtensions()
		require.NoError(t, err, "error starting test server")
		return httptest.NewServer(f.mux)
	}

	startBackendTestSrv := func(response string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range r.Header {
				w.Header().Add(k, strings.Join(v, ","))
			}
			fmt.Fprintln(w, response)
		}))
	}
	newExtensionRequest := func(t *testing.T, method, url string) *http.Request {
		t.Helper()
		r, err := http.NewRequest(method, url, http.NoBody)
		require.NoError(t, err, "error initializing request")
		r.Header.Add(extension.HeaderArgoCDApplicationName, "namespace:app-name")
		r.Header.Add(extension.HeaderArgoCDProjectName, defaultProjectName)
		return r
	}

	t.Run("will call extension backend successfully", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		backendResponse := "some data"
		backendEndpoint := "some-backend"
		clusterURL := "some-url"
		backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range r.Header {
				w.Header().Add(k, strings.Join(v, ","))
			}
			fmt.Fprintln(w, backendResponse)
		}))
		defer backendSrv.Close()
		withRbac(f, true, true)
		withUser(f, "some-user", []string{"group1", "group2"})
		withExtensionConfig(getExtensionConfig(backendEndpoint, backendSrv.URL), f)
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, backendEndpoint))
		app := getApp("", clusterURL, defaultProjectName)
		proj := getProjectWithDestinations("project-name", nil, []string{clusterURL})
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(app, nil)
		withProject(proj, f)
		var wg sync.WaitGroup
		wg.Add(2)
		f.metricsMock.
			On("IncExtensionRequestCounter", mock.Anything, mock.Anything).
			Run(func(_ mock.Arguments) {
				wg.Done()
			})
		f.metricsMock.
			On("ObserveExtensionRequestDuration", mock.Anything, mock.Anything).
			Run(func(_ mock.Arguments) {
				wg.Done()
			})

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		actual := strings.TrimSuffix(string(body), "\n")
		assert.Equal(t, backendResponse, actual)
		assert.Equal(t, defaultServerNamespace, resp.Header.Get(extension.HeaderArgoCDNamespace))
		assert.Equal(t, clusterURL, resp.Header.Get(extension.HeaderArgoCDTargetClusterURL))
		assert.Equal(t, "Bearer some-bearer-token", resp.Header.Get("Authorization"))
		assert.Equal(t, "some-user", resp.Header.Get(extension.HeaderArgoCDUsername))
		assert.Equal(t, "group1,group2", resp.Header.Get(extension.HeaderArgoCDGroups))

		// waitgroup is necessary to make sure assertions aren't executed before
		// the goroutine initiated by extension.CallExtension concludes which would
		// lead to flaky test.
		wg.Wait()
		f.metricsMock.AssertCalled(t, "IncExtensionRequestCounter", backendEndpoint, http.StatusOK)
		f.metricsMock.AssertCalled(t, "ObserveExtensionRequestDuration", backendEndpoint, mock.Anything)
	})
	t.Run("proxy will return 404 if extension endpoint not registered", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		withExtensionConfig(getExtensionConfigString(), f)
		withRbac(f, true, true)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		cluster1Name := "cluster1"
		f.appGetterMock.On("Get", "namespace", "app-name").Return(getApp(cluster1Name, "", defaultProjectName), nil)
		withProject(getProjectWithDestinations("project-name", []string{cluster1Name}, []string{"some-url"}), f)

		ts := startTestServer(t, f)
		defer ts.Close()
		nonRegistered := "non-registered"
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, nonRegistered))

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("will route requests with 2 backends for the same extension successfully", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		extName := "some-extension"

		response1 := "response backend 1"
		cluster1Name := "cluster1"
		cluster1URL := "url1"
		beSrv1 := startBackendTestSrv(response1)
		defer beSrv1.Close()

		response2 := "response backend 2"
		cluster2Name := "cluster2"
		cluster2URL := "url2"
		beSrv2 := startBackendTestSrv(response2)
		defer beSrv2.Close()

		f.appGetterMock.On("Get", "ns1", "app1").Return(getApp(cluster1Name, "", defaultProjectName), nil)
		f.appGetterMock.On("Get", "ns2", "app2").Return(getApp("", cluster2URL, defaultProjectName), nil)

		withRbac(f, true, true)
		withExtensionConfig(getExtensionConfigWith2Backends(extName, beSrv1.URL, cluster1Name, cluster1URL, beSrv2.URL, cluster2Name, cluster2URL), f)
		withProject(getProjectWithDestinations("project-name", []string{cluster1Name}, []string{cluster2URL}), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})

		ts := startTestServer(t, f)
		defer ts.Close()

		url := fmt.Sprintf("%s/extensions/%s/", ts.URL, extName)
		req := newExtensionRequest(t, http.MethodGet, url)
		req.Header.Del(extension.HeaderArgoCDApplicationName)

		req1 := req.Clone(t.Context())
		req1.Header.Add(extension.HeaderArgoCDApplicationName, "ns1:app1")
		req2 := req.Clone(t.Context())
		req2.Header.Add(extension.HeaderArgoCDApplicationName, "ns2:app2")

		// when
		resp1, err := http.DefaultClient.Do(req1)
		require.NoError(t, err)
		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)

		// then
		require.NotNil(t, resp1)
		assert.Equal(t, http.StatusOK, resp1.StatusCode)
		body, err := io.ReadAll(resp1.Body)
		require.NoError(t, err)
		actual := strings.TrimSuffix(string(body), "\n")
		assert.Equal(t, response1, actual)
		assert.Equal(t, "Bearer some-bearer-token", resp1.Header.Get("Authorization"))

		require.NotNil(t, resp2)
		assert.Equal(t, http.StatusOK, resp2.StatusCode)
		body, err = io.ReadAll(resp2.Body)
		require.NoError(t, err)
		actual = strings.TrimSuffix(string(body), "\n")
		assert.Equal(t, response2, actual)
		assert.Equal(t, "Bearer another-bearer-token", resp2.Header.Get("Authorization"))
	})
	t.Run("will return 401 if sub has no access to get application", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		allowApp := false
		allowExtension := true
		extName := "some-extension"
		withRbac(f, allowApp, allowExtension)
		withExtensionConfig(getExtensionConfig(extName, "http://fake"), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, extName))
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(getApp("", "", defaultProjectName), nil)

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("will return 401 if sub has no access to invoke extension", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		allowApp := true
		allowExtension := false
		extName := "some-extension"
		withRbac(f, allowApp, allowExtension)
		withExtensionConfig(getExtensionConfig(extName, "http://fake"), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, extName))
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(getApp("", "", defaultProjectName), nil)

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("will return 401 if project has no access to target cluster", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		allowApp := true
		allowExtension := true
		extName := "some-extension"
		noCluster := []string{}
		withRbac(f, allowApp, allowExtension)
		withExtensionConfig(getExtensionConfig(extName, "http://fake"), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, extName))
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(getApp("", "", defaultProjectName), nil)
		proj := getProjectWithDestinations("project-name", nil, noCluster)
		withProject(proj, f)

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("will return 401 if project in application does not exist", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		allowApp := true
		allowExtension := true
		extName := "some-extension"
		withRbac(f, allowApp, allowExtension)
		withExtensionConfig(getExtensionConfig(extName, "http://fake"), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, extName))
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(getApp("", "", defaultProjectName), nil)
		f.projMock.On("Get", defaultProjectName).Return(nil, nil)

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("will return 401 if project in application does not match with header", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		allowApp := true
		allowExtension := true
		extName := "some-extension"
		differentProject := "differentProject"
		withRbac(f, allowApp, allowExtension)
		withExtensionConfig(getExtensionConfig(extName, "http://fake"), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", fmt.Sprintf("%s/extensions/%s/", ts.URL, extName))
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(getApp("", "", differentProject), nil)

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("will return 401 if application defines name and server destination", func(t *testing.T) {
		// This test is to validate a security risk with malicious application
		// trying to gain access to execute extensions in clusters it doesn't
		// have access.

		// given
		t.Parallel()
		f := setup()
		extName := "some-extension"
		maliciousName := "srv1"
		destinationServer := "some-valid-server"

		f.appGetterMock.On("Get", "ns1", "app1").Return(getApp(maliciousName, destinationServer, defaultProjectName), nil)

		withRbac(f, true, true)
		withExtensionConfig(getExtensionConfigWith2Backends(extName, "url1", "cluster1Name", "cluster1URL", "url2", "cluster2Name", "cluster2URL"), f)
		withProject(getProjectWithDestinations("project-name", nil, []string{"srv1", destinationServer}), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})

		ts := startTestServer(t, f)
		defer ts.Close()

		url := fmt.Sprintf("%s/extensions/%s/", ts.URL, extName)
		req := newExtensionRequest(t, http.MethodGet, url)
		req.Header.Del(extension.HeaderArgoCDApplicationName)
		req1 := req.Clone(t.Context())
		req1.Header.Add(extension.HeaderArgoCDApplicationName, "ns1:app1")

		// when
		resp1, err := http.DefaultClient.Do(req1)
		require.NoError(t, err)

		// then
		require.NotNil(t, resp1)
		assert.Equal(t, http.StatusUnauthorized, resp1.StatusCode)
		body, err := io.ReadAll(resp1.Body)
		require.NoError(t, err)
		actual := strings.TrimSuffix(string(body), "\n")
		assert.Equal(t, "Unauthorized extension request", actual)
	})
	t.Run("will return 400 if no extension name is provided", func(t *testing.T) {
		// given
		t.Parallel()
		f := setup()
		allowApp := true
		allowExtension := true
		extName := "some-extension"
		differentProject := "differentProject"
		withRbac(f, allowApp, allowExtension)
		withExtensionConfig(getExtensionConfig(extName, "http://fake"), f)
		withMetrics(f)
		withUser(f, "some-user", []string{"group1", "group2"})
		ts := startTestServer(t, f)
		defer ts.Close()
		r := newExtensionRequest(t, "Get", ts.URL+"/extensions/")
		f.appGetterMock.On("Get", mock.Anything, mock.Anything).Return(getApp("", "", differentProject), nil)

		// when
		resp, err := http.DefaultClient.Do(r)

		// then
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func getExtensionConfig(name, url string) string {
	cfg := `
extensions:
- name: %s
  backend:
    services:
    - url: %s
      headers:
      - name: Authorization
        value: '$extension.auth.header'
`
	return fmt.Sprintf(cfg, name, url)
}

func getExtensionConfigWith2Backends(name, url1, clus1Name, clus1URL, url2, clus2Name, clus2URL string) string {
	cfg := `
extensions:
- name: %s
  backend:
    services:
    - url: %s
      headers:
      - name: Authorization
        value: '$extension.auth.header'
      cluster:
        name: %s
        server: %s
    - url: %s
      headers:
      - name: Authorization
        value: '$extension.auth.header2'
      cluster:
        name: %s
        server: %s
    - url: http://test.com
      cluster:
        name: cl1
    - url: http://test2.com
      cluster:
        name: cl2
`
	return fmt.Sprintf(cfg, name, url1, clus1Name, clus1URL, url2, clus2Name, clus2URL)
}

func getExtensionConfigString() string {
	return `
extensions:
- name: external-backend
  backend:
    connectionTimeout: 10s
    keepAlive: 11s
    idleConnectionTimeout: 12s
    maxIdleConnections: 30
    services:
    - url: https://httpbin.org
      headers:
      - name: some-header
        value: '$some.secret.ref'
- name: some-backend
  backend:
    services:
    - url: http://localhost:7777
`
}

func getSingleExtensionConfigString() string {
	return `
connectionTimeout: 10s
keepAlive: 11s
idleConnectionTimeout: 12s
maxIdleConnections: 30
services:
- url: http://localhost:7777
`
}

func getExtensionConfigNoService() string {
	return `
extensions:
- backend:
    connectionTimeout: 2s
`
}

func getExtensionConfigNoName() string {
	return `
extensions:
- backend:
    services:
    - url: https://httpbin.org
`
}

func getExtensionConfigInvalidName() string {
	return `
extensions:
- name: invalid/name
  backend:
    services:
    - url: https://httpbin.org
`
}

func getExtensionConfigNoURL() string {
	return `
extensions:
- name: some-backend
  backend:
    services:
    - cluster: some-cluster
`
}

func getExtensionConfigNoHeaderName() string {
	return `
extensions:
- name: some-extension
  backend:
    services:
    - url: https://httpbin.org
      headers:
      - value: '$some.secret.key'
`
}

func getExtensionConfigNoHeaderValue() string {
	return `
extensions:
- name: some-extension
  backend:
    services:
    - url: https://httpbin.org
      headers:
      - name: some-header-name
`
}
