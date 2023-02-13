package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/kubescape/kubevuln/adapters"
	"github.com/kubescape/kubevuln/controllers"
	"github.com/kubescape/kubevuln/core/services"
	"github.com/kubescape/kubevuln/internal/tools"
	"github.com/kubescape/kubevuln/repositories"
	"gotest.tools/v3/assert"
)

func TestScan(t *testing.T) {
	tests := []struct {
		name         string
		yamlFile     string
		expectedCode int
		expectedBody string
	}{
		{
			"valid scan command succeeds and reports CVE",
			"../../api/v1/testdata/scan.yaml",
			200,
			"{\"data\":\"new CVE manifest created\",\"status\":\"success\"}",
		},
		{
			"missing fields",
			"../../api/v1/testdata/scan-incomplete.yaml",
			500,
			"null",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := repositories.NewMemoryStorage()
			sbomAdapter := adapters.NewMockSBOMAdapter()
			cveAdapter := adapters.NewMockCVEAdapter()
			platform := adapters.NewMockPlatform()
			service := services.NewScanService(sbomAdapter, repository, cveAdapter, repository, platform)
			controller := controllers.NewHTTPController(service)

			router := gin.Default()

			router.POST("/v1/generateSBOM", controller.GenerateSBOM)
			router.POST("/v1/scanImage", controller.ScanCVE)

			file, err := os.Open(test.yamlFile)
			tools.EnsureSetup(t, err == nil)
			req, _ := http.NewRequest("POST", "/v1/generateSBOM", file)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			file, err = os.Open(test.yamlFile)
			tools.EnsureSetup(t, err == nil)
			req, _ = http.NewRequest("POST", "/v1/scanImage", file)
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Assert(t, test.expectedCode == w.Code)
			assert.Assert(t, test.expectedBody == w.Body.String())
		})
	}
}
