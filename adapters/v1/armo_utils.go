package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/armosec/armoapi-go/apis"
	"github.com/armosec/armoapi-go/armotypes"
	"github.com/armosec/cluster-container-scanner-api/containerscan"
	"github.com/armosec/cluster-container-scanner-api/containerscan/v1"
	"github.com/armosec/utils-go/httputils"
	"github.com/armosec/utils-k8s-go/armometadata"
	"github.com/kubescape/go-logger"
	"github.com/kubescape/go-logger/helpers"
	"github.com/kubescape/kubevuln/core/domain"
)

func sendSummaryAndVulnerabilities(report *v1.ScanResultReport, eventReceiverURL string, totalVulnerabilities int, scanID string, firstVulnerabilitiesChunk []containerscan.CommonContainerVulnerabilityResult, errChan chan<- error, sendWG *sync.WaitGroup) (nextPartNum int) {
	//get the first chunk
	firstChunkVulnerabilitiesCount := len(firstVulnerabilitiesChunk)
	//prepare summary report
	//nextPartNum = 0
	//summaryReport := &v1.ScanResultReport{
	//	PaginationInfo:  wssc.PaginationMarks{ReportNumber: nextPartNum},
	//	Summary:         report.Summarize(),
	//	ContainerScanID: scanID,
	//	Timestamp:       report.Timestamp,
	//	Designators:     report.Designators,
	//}
	//if size of summary + first chunk does not exceed max size
	if httputils.JSONSize(report)+httputils.JSONSize(firstVulnerabilitiesChunk) <= maxBodySize {
		//then post the summary report with the first vulnerabilities chunk
		report.Vulnerabilities = firstVulnerabilitiesChunk
		//if all vulnerabilities got into the first chunk set this as the last report
		report.PaginationInfo.IsLastReport = totalVulnerabilities == firstChunkVulnerabilitiesCount
		//first chunk sent (or is nil) so set to nil
		firstVulnerabilitiesChunk = nil
	} else {
		//first chunk is not included in the summary, so if there are vulnerabilities to send set the last part to false
		report.PaginationInfo.IsLastReport = firstChunkVulnerabilitiesCount != 0
	}
	//send the summary report
	postResultsAsGoroutine(report, eventReceiverURL, report.Summary.ImageTag, report.Summary.WLID, errChan, sendWG)
	nextPartNum++
	//send the first chunk if it was not sent yet (because of summary size)
	if firstVulnerabilitiesChunk != nil {
		postResultsAsGoroutine(&v1.ScanResultReport{
			PaginationInfo:  apis.PaginationMarks{ReportNumber: nextPartNum, IsLastReport: totalVulnerabilities == firstChunkVulnerabilitiesCount},
			Vulnerabilities: firstVulnerabilitiesChunk,
			ContainerScanID: scanID,
			Timestamp:       report.Timestamp,
			Designators:     report.Designators,
		}, eventReceiverURL, report.Summary.ImageTag, report.Summary.WLID, errChan, sendWG)
		nextPartNum++
	}
	return nextPartNum
}

func postResultsAsGoroutine(report *v1.ScanResultReport, eventReceiverURL, imagetag string, wlid string, errorChan chan<- error, wg *sync.WaitGroup) {
	wg.Add(1)
	go func(report *v1.ScanResultReport, eventReceiverURL, imagetag string, wlid string, errorChan chan<- error, wg *sync.WaitGroup) {
		defer wg.Done()
		postResults(report, eventReceiverURL, imagetag, wlid, errorChan)
	}(report, eventReceiverURL, imagetag, wlid, errorChan, wg)
}

func postResults(report *v1.ScanResultReport, eventReceiverURL, imagetag string, wlid string, errorChan chan<- error) {
	payload, err := json.Marshal(report)
	if err != nil {
		logger.L().Error("failed to convert to json", helpers.Error(err))
		errorChan <- err
		return
	}
	//if printPostJSON != "" {
	//	logger.L().Info(fmt.Sprintf("printPostJSON: %s", payload))
	//}
	urlBase, err := url.Parse(eventReceiverURL)
	if err != nil {
		err = fmt.Errorf("fail parsing URL, %s, err: %s", eventReceiverURL, err.Error())
		logger.L().Error(err.Error(), helpers.Error(err))
		errorChan <- err
		return
	}

	urlBase.Path = "k8s/v2/containerScan"
	q := urlBase.Query()
	q.Add(armotypes.CustomerGuidQuery, report.Designators.Attributes[armotypes.AttributeCustomerGUID])
	urlBase.RawQuery = q.Encode()

	resp, err := httputils.HttpPost(http.DefaultClient, urlBase.String(), map[string]string{"Content-Type": "application/json"}, payload)
	if err != nil {
		logger.L().Error(fmt.Sprintf("fail posting to event receiver image %s wlid %s", imagetag, wlid), helpers.Error(err))
		errorChan <- err
		return
	}
	defer resp.Body.Close()
	body, err := httputils.HttpRespToString(resp)
	if err != nil {
		logger.L().Error("Vulnerabilities post to event receiver failed", helpers.Error(err), helpers.String("body", body))
		errorChan <- err
		return
	}
	logger.L().Info(fmt.Sprintf("posting to event receiver image %s wlid %s finished successfully response body: %s", imagetag, wlid, body)) // systest dependent
}

func sendVulnerabilitiesRoutine(chunksChan <-chan []containerscan.CommonContainerVulnerabilityResult, eventReceiverURL string, scanID string, finalReport v1.ScanResultReport, errChan chan error, sendWG *sync.WaitGroup, totalVulnerabilities int, firstChunkVulnerabilitiesCount int, nextPartNum int) {
	go func(scanID string, finalReport v1.ScanResultReport, errorChan chan<- error, sendWG *sync.WaitGroup, expectedVulnerabilitiesSum int, partNum int) {
		sendVulnerabilities(chunksChan, eventReceiverURL, partNum, expectedVulnerabilitiesSum, scanID, finalReport, errorChan, sendWG)
		//wait for all post request to end (including summary report)
		sendWG.Wait()
		//no more post requests - close the error channel
		close(errorChan)
	}(scanID, finalReport, errChan, sendWG, totalVulnerabilities-firstChunkVulnerabilitiesCount, nextPartNum)
}

func sendVulnerabilities(chunksChan <-chan []containerscan.CommonContainerVulnerabilityResult, eventReceiverURL string, partNum int, expectedVulnerabilitiesSum int, scanID string, finalReport v1.ScanResultReport, errorChan chan<- error, sendWG *sync.WaitGroup) {
	//post each vulnerability chunk in a different report
	chunksVulnerabilitiesCount := 0
	for vulnerabilities := range chunksChan {
		chunksVulnerabilitiesCount += len(vulnerabilities)
		postResultsAsGoroutine(&v1.ScanResultReport{
			PaginationInfo:  apis.PaginationMarks{ReportNumber: partNum, IsLastReport: chunksVulnerabilitiesCount == expectedVulnerabilitiesSum},
			Vulnerabilities: vulnerabilities,
			ContainerScanID: scanID,
			Timestamp:       finalReport.Timestamp,
			Designators:     finalReport.Designators,
		}, eventReceiverURL, finalReport.Summary.ImageTag, finalReport.Summary.WLID, errorChan, sendWG)
		partNum++
	}

	//verify that all vulnerabilities received and sent
	if chunksVulnerabilitiesCount != expectedVulnerabilitiesSum {
		errorChan <- fmt.Errorf("error while splitting vulnerabilities chunks, expected " + strconv.Itoa(expectedVulnerabilitiesSum) +
			" vulnerabilities but received " + strconv.Itoa(chunksVulnerabilitiesCount))
	}
}

func incrementCounter(counter *int64, isGlobal, isIgnored bool) {
	if isGlobal && isIgnored {
		return
	}
	*counter++
}

func summarize(report v1.ScanResultReport, workload domain.ScanCommand, hasRelevancy bool) *containerscan.CommonContainerScanSummaryResult {
	summary := containerscan.CommonContainerScanSummaryResult{
		Designators:      report.Designators,
		SeverityStats:    containerscan.SeverityStats{},
		CustomerGUID:     report.Designators.Attributes[armotypes.AttributeCustomerGUID],
		ContainerScanID:  report.ContainerScanID,
		WLID:             workload.Wlid,
		ImageID:          workload.ImageHash,
		ImageTag:         workload.ImageTag,
		ClusterName:      report.Designators.Attributes[armotypes.AttributeCluster],
		Namespace:        report.Designators.Attributes[armotypes.AttributeNamespace],
		ContainerName:    report.Designators.Attributes[armotypes.AttributeContainerName],
		JobIDs:           workload.Session.JobIDs,
		Timestamp:        report.Timestamp,
		HasRelevancyData: hasRelevancy,
	}

	imageInfo, err := armometadata.ImageTagToImageInfo(workload.ImageTag)
	if err == nil {
		summary.Registry = imageInfo.Registry
		summary.Version = imageInfo.VersionImage
	}

	summary.PackagesName = make([]string, 0)

	actualSeveritiesStats := map[string]containerscan.SeverityStats{}
	exculdedSeveritiesStats := map[string]containerscan.SeverityStats{}

	vulnsList := make([]containerscan.ShortVulnerabilityResult, 0)

	for _, vul := range report.Vulnerabilities {
		isIgnored := len(vul.ExceptionApplied) > 0 &&
			len(vul.ExceptionApplied[0].Actions) > 0 &&
			vul.ExceptionApplied[0].Actions[0] == armotypes.Ignore

		severitiesStats := exculdedSeveritiesStats
		if !isIgnored {
			summary.TotalCount++
			vulnsList = append(vulnsList, *(vul.ToShortVulnerabilityResult()))
			severitiesStats = actualSeveritiesStats
		}

		// TODO: maybe add all severities just to have a placeholders
		if !containerscan.KnownSeverities[vul.Severity] {
			vul.Severity = containerscan.UnknownSeverity
		}

		vulnSeverityStats, ok := severitiesStats[vul.Severity]
		if !ok {
			vulnSeverityStats = containerscan.SeverityStats{Severity: vul.Severity}
		}

		vulnSeverityStats.TotalCount++
		isFixed := containerscan.CalculateFixed(vul.Fixes) > 0
		if isFixed {
			vulnSeverityStats.FixAvailableOfTotalCount++
			incrementCounter(&summary.FixAvailableOfTotalCount, true, isIgnored)
		}
		isRCE := vul.IsRCE()
		if isRCE {
			vulnSeverityStats.RCECount++
			incrementCounter(&summary.RCECount, true, isIgnored)
			if isFixed {
				summary.RCEFixCount++
				vulnSeverityStats.RCEFixCount++
			}
		}
		if vul.IsRelevant != nil && *vul.IsRelevant {
			vulnSeverityStats.RelevantCount++
			incrementCounter(&summary.RelevantCount, true, isIgnored)
			if isFixed {
				vulnSeverityStats.FixAvailableForRelevantCount++
				incrementCounter(&summary.FixAvailableForRelevantCount, true, isIgnored)
			}

		}
		severitiesStats[vul.Severity] = vulnSeverityStats
	}

	summary.Status = "Success"
	summary.Vulnerabilities = vulnsList

	for sever := range actualSeveritiesStats {
		summary.SeveritiesStats = append(summary.SeveritiesStats, actualSeveritiesStats[sever])
	}
	for sever := range exculdedSeveritiesStats {
		summary.ExcludedSeveritiesStats = append(summary.ExcludedSeveritiesStats, exculdedSeveritiesStats[sever])
	}

	return &summary
}
