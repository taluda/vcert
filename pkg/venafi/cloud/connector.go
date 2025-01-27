/*
 * Copyright 2018 Venafi, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cloud

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	netUrl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Venafi/vcert/v4/pkg/verror"

	"github.com/Venafi/vcert/v4/pkg/certificate"
	"github.com/Venafi/vcert/v4/pkg/endpoint"
)

type urlResource string

const (
	apiURL                                        = "api.venafi.cloud/"
	apiVersion                                    = "v1/"
	basePath                                      = "outagedetection/" + apiVersion
	urlResourceUserAccounts           urlResource = apiVersion + "useraccounts"
	urlResourceCertificateRequests    urlResource = basePath + "certificaterequests"
	urlResourceCertificateStatus                  = urlResourceCertificateRequests + "/%s"
	urlResourceCertificates           urlResource = basePath + "certificates"
	urlResourceCertificateByID                    = urlResourceCertificates + "/%s"
	urlResourceCertificateRetrievePem             = urlResourceCertificates + "/%s/contents"
	urlResourceCertificateSearch      urlResource = basePath + "certificatesearch"
	urlResourceTemplate               urlResource = basePath + "applications/%s/certificateissuingtemplates/%s"
	urlAppDetailsByName               urlResource = basePath + "applications/name/%s"

	defaultAppName = "Default"
)

type condorChainOption string

const (
	condorChainOptionRootFirst condorChainOption = "ROOT_FIRST"
	condorChainOptionRootLast  condorChainOption = "EE_FIRST"
)

// Connector contains the base data needed to communicate with the Venafi Cloud servers
type Connector struct {
	baseURL string
	apiKey  string
	verbose bool
	user    *userDetails
	trust   *x509.CertPool
	zone    cloudZone
	client  *http.Client
}

// NewConnector creates a new Venafi Cloud Connector object used to communicate with Venafi Cloud
func NewConnector(url string, zone string, verbose bool, trust *x509.CertPool) (*Connector, error) {
	cZone := cloudZone{zone: zone}
	c := Connector{verbose: verbose, trust: trust, zone: cZone}

	var err error
	c.baseURL, err = normalizeURL(url)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

//normalizeURL allows overriding the default URL used to communicate with Venafi Cloud
func normalizeURL(url string) (normalizedURL string, err error) {
	if url == "" {
		url = apiURL
		//return "", fmt.Errorf("base URL cannot be empty")
	}
	modified := strings.ToLower(url)
	reg := regexp.MustCompile("^http(|s)://")
	if reg.FindStringIndex(modified) == nil {
		modified = "https://" + modified
	} else {
		modified = reg.ReplaceAllString(modified, "https://")
	}
	if !strings.HasSuffix(modified, "/") {
		modified = modified + "/"
	}
	normalizedURL = modified
	return normalizedURL, nil
}

func (c *Connector) SetZone(z string) {
	cZone := cloudZone{zone: z}
	c.zone = cZone
}

func (c *Connector) GetType() endpoint.ConnectorType {
	return endpoint.ConnectorTypeCloud
}

// Ping attempts to connect to the Venafi Cloud API and returns an errror if it cannot
func (c *Connector) Ping() (err error) {

	return nil
}

// Authenticate authenticates the user with Venafi Cloud using the provided API Key
func (c *Connector) Authenticate(auth *endpoint.Authentication) (err error) {
	if auth == nil {
		return fmt.Errorf("failed to authenticate: missing credentials")
	}
	c.apiKey = auth.APIKey
	url := c.getURL(urlResourceUserAccounts)
	statusCode, status, body, err := c.request("GET", url, nil, true)
	if err != nil {
		return err
	}
	ud, err := parseUserDetailsResult(http.StatusOK, statusCode, status, body)
	if err != nil {
		return
	}
	c.user = ud
	return
}

func (c *Connector) ReadPolicyConfiguration() (policy *endpoint.Policy, err error) {
	config, err := c.ReadZoneConfiguration()
	if err != nil {
		return nil, err
	}
	policy = &config.Policy
	return
}

// ReadZoneConfiguration reads the Zone information needed for generating and requesting a certificate from Venafi Cloud
func (c *Connector) ReadZoneConfiguration() (config *endpoint.ZoneConfiguration, err error) {
	template, err := c.getTemplateByID()
	if err != nil {
		return
	}
	config = getZoneConfiguration(template)
	return config, nil
}

// RequestCertificate submits the CSR to the Venafi Cloud API for processing
func (c *Connector) RequestCertificate(req *certificate.Request) (requestID string, err error) {
	if req.CsrOrigin == certificate.ServiceGeneratedCSR {
		return "", fmt.Errorf("service generated CSR is not supported by Saas service")
	}

	url := c.getURL(urlResourceCertificateRequests)
	if c.user == nil || c.user.Company == nil {
		return "", fmt.Errorf("must be autheticated to request a certificate")
	}

	ipAddr := endpoint.LocalIP
	origin := endpoint.SDKName
	for _, f := range req.CustomFields {
		if f.Type == certificate.CustomFieldOrigin {
			origin = f.Value
		}
	}

	appDetails, err := c.getAppDetailsByName(c.zone.getApplicationName())
	if err != nil {
		return "", err
	}
	templateId := appDetails.CitAliasToIdMap[c.zone.getTemplateAlias()]

	cloudReq := certificateRequest{
		CSR:           string(req.GetCSR()),
		ApplicationId: appDetails.ApplicationId,
		TemplateId:    templateId,
		ApiClientInformation: certificateRequestClientInfo{
			Type:       origin,
			Identifier: ipAddr,
		},
	}

	if req.Location != nil {
		workload := req.Location.Workload
		if workload == "" {
			workload = defaultAppName
		}
		nodeName := req.Location.Instance
		appName := workload

		cloudReq.CertificateUsageMetadata = []certificateUsageMetadata{
			{
				AppName:  appName,
				NodeName: nodeName,
			},
		}
	}

	if req.ValidityHours > 0 {
		hoursStr := strconv.Itoa(req.ValidityHours)
		validityHoursStr := "PT" + hoursStr + "H"
		cloudReq.ValidityPeriod = validityHoursStr
	}

	statusCode, status, body, err := c.request("POST", url, cloudReq)

	if err != nil {
		return "", err
	}
	cr, err := parseCertificateRequestResult(statusCode, status, body)
	if err != nil {
		return "", err
	}
	requestID = cr.CertificateRequests[0].ID
	req.PickupID = requestID
	return requestID, nil
}

func (c *Connector) getCertificateStatus(requestID string) (certStatus *certificateStatus, err error) {
	url := c.getURL(urlResourceCertificateStatus)
	url = fmt.Sprintf(url, requestID)
	statusCode, _, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusOK {
		certStatus = &certificateStatus{}
		err = json.Unmarshal(body, certStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to parse certificate request status response: %s", err)
		}
		return
	}
	respErrors, err := parseResponseErrors(body)
	if err == nil {
		respError := fmt.Sprintf("Unexpected status code on Venafi Cloud certificate search. Status: %d\n", statusCode)
		for _, e := range respErrors {
			respError += fmt.Sprintf("Error Code: %d Error: %s\n", e.Code, e.Message)
		}
		return nil, fmt.Errorf(respError)
	}

	return nil, fmt.Errorf("unexpected status code on Venafi Cloud certificate search. Status: %d", statusCode)

}

// RetrieveCertificate retrieves the certificate for the specified ID
func (c *Connector) RetrieveCertificate(req *certificate.Request) (certificates *certificate.PEMCollection, err error) {

	if req.FetchPrivateKey {
		return nil, fmt.Errorf("failed to retrieve private key from Venafi Cloud service: not supported")
	}
	if req.PickupID == "" && req.CertID == "" && req.Thumbprint != "" {
		// search cert by Thumbprint and fill pickupID
		var certificateRequestId string
		searchResult, err := c.searchCertificatesByFingerprint(req.Thumbprint)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve certificate: %s", err)
		}
		if len(searchResult.Certificates) == 0 {
			return nil, fmt.Errorf("no certifiate found using fingerprint %s", req.Thumbprint)
		}

		var reqIds []string
		isOnlyOneCertificateRequestId := true
		for _, c := range searchResult.Certificates {
			reqIds = append(reqIds, c.CertificateRequestId)
			if certificateRequestId != "" && certificateRequestId != c.CertificateRequestId {
				isOnlyOneCertificateRequestId = false
			}
			if c.CertificateRequestId != "" {
				certificateRequestId = c.CertificateRequestId
			}
			if c.Id != "" {
				req.CertID = c.Id
			}
		}
		if !isOnlyOneCertificateRequestId {
			return nil, fmt.Errorf("more than one CertificateRequestId was found with the same Fingerprint: %s", reqIds)
		}

		req.PickupID = certificateRequestId
	}

	startTime := time.Now()
	//Wait for certificate to be issued by checking it's PickupID
	//If certID is filled then certificate should be already issued.
	var certificateId string
	if req.CertID == "" {
		for {
			if req.PickupID == "" {
				break
			}
			certStatus, err := c.getCertificateStatus(req.PickupID)
			if err != nil {
				return nil, fmt.Errorf("unable to retrieve: %s", err)
			}
			if certStatus.Status == "ISSUED" {
				certificateId = certStatus.CertificateIdsList[0]
				break // to fetch the cert itself
			} else if certStatus.Status == "FAILED" {
				return nil, fmt.Errorf("failed to retrieve certificate. Status: %v", certStatus)
			}
			// status.Status == "REQUESTED" || status.Status == "PENDING"
			if req.Timeout == 0 {
				return nil, endpoint.ErrCertificatePending{CertificateID: req.PickupID, Status: certStatus.Status}
			}
			if time.Now().After(startTime.Add(req.Timeout)) {
				return nil, endpoint.ErrRetrieveCertificateTimeout{CertificateID: req.PickupID}
			}
			// fmt.Printf("pending... %s\n", status.Status)
			time.Sleep(2 * time.Second)
		}
	} else {
		certificateId = req.CertID
	}

	if c.user == nil || c.user.Company == nil {
		return nil, fmt.Errorf("must be autheticated to retieve certificate")
	}

	url := c.getURL(urlResourceCertificateRetrievePem)
	url = fmt.Sprintf(url, certificateId)

	switch {
	case req.CertID != "":
		statusCode, status, body, err := c.request("GET", url, nil)
		if err != nil {
			return nil, err
		}
		if statusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to retrieve certificate. StatusCode: %d -- Status: %s -- Server Data: %s", statusCode, status, body)
		}
		return newPEMCollectionFromResponse(body, certificate.ChainOptionIgnore)
	case req.PickupID != "":
		url += "?chainOrder=%s&format=PEM"
		switch req.ChainOption {
		case certificate.ChainOptionRootFirst:
			url = fmt.Sprintf(url, condorChainOptionRootFirst)
		default:
			url = fmt.Sprintf(url, condorChainOptionRootLast)
		}
		statusCode, status, body, err := c.request("GET", url, nil)
		if err != nil {
			return nil, err
		}
		if statusCode == http.StatusOK {
			certificates, err = newPEMCollectionFromResponse(body, req.ChainOption)
			if err != nil {
				return nil, err
			}
			err = req.CheckCertificate(certificates.Certificate)
			return certificates, err
		} else if statusCode == http.StatusConflict { // Http Status Code 409 means the certificate has not been signed by the ca yet.
			return nil, endpoint.ErrCertificatePending{CertificateID: req.PickupID}
		} else {
			return nil, fmt.Errorf("failed to retrieve certificate. StatusCode: %d -- Status: %s", statusCode, status)
		}
	}
	return nil, fmt.Errorf("couldn't retrieve certificate because both PickupID and CertId are empty")
}

// RevokeCertificate attempts to revoke the certificate
func (c *Connector) RevokeCertificate(revReq *certificate.RevocationRequest) (err error) {
	return fmt.Errorf("not supported by endpoint")
}

// RenewCertificate attempts to renew the certificate
func (c *Connector) RenewCertificate(renewReq *certificate.RenewalRequest) (requestID string, err error) {

	/* 1st step is to get CertificateRequestId which is required to lookup managedCertificateId and zoneId */
	var certificateRequestId string

	if renewReq.Thumbprint != "" {
		// by Thumbprint (aka Fingerprint)
		searchResult, err := c.searchCertificatesByFingerprint(renewReq.Thumbprint)
		if err != nil {
			return "", fmt.Errorf("failed to create renewal request: %s", err)
		}
		if len(searchResult.Certificates) == 0 {
			return "", fmt.Errorf("no certifiate found using fingerprint %s", renewReq.Thumbprint)
		}

		var reqIds []string
		isOnlyOneCertificateRequestId := true
		for _, c := range searchResult.Certificates {
			reqIds = append(reqIds, c.CertificateRequestId)
			if certificateRequestId != "" && certificateRequestId != c.CertificateRequestId {
				isOnlyOneCertificateRequestId = false
			}
			certificateRequestId = c.CertificateRequestId
		}
		if !isOnlyOneCertificateRequestId {
			return "", fmt.Errorf("error: more than one CertificateRequestId was found with the same Fingerprint: %s", reqIds)
		}
	} else if renewReq.CertificateDN != "" {
		// by CertificateDN (which is the same as CertificateRequestId for current implementation)
		certificateRequestId = renewReq.CertificateDN
	} else {
		return "", fmt.Errorf("failed to create renewal request: CertificateDN or Thumbprint required")
	}

	/* 2nd step is to get ManagedCertificateId & ZoneId by looking up certificate request record */
	previousRequest, err := c.getCertificateStatus(certificateRequestId)
	if err != nil {
		return "", fmt.Errorf("certificate renew failed: %s", err)
	}
	applicationId := previousRequest.ApplicationId
	templateId := previousRequest.TemplateId
	certificateId := previousRequest.CertificateIdsList[0]

	emptyField := ""
	if certificateId == "" {
		emptyField = "certificateId"
	} else if applicationId == "" {
		emptyField = "applicationId"
	} else if templateId == "" {
		emptyField = "templateId"
	}
	if emptyField != "" {
		return "", fmt.Errorf("failed to submit renewal request for certificate: %s is empty, certificate status is %s", emptyField, previousRequest.Status)
	}

	/* 3rd step is to get Certificate Object by id
	   and check if latestCertificateRequestId there equals to certificateRequestId from 1st step */
	managedCertificate, err := c.getCertificate(certificateId)
	if err != nil {
		return "", fmt.Errorf("failed to renew certificate: %s", err)
	}
	if managedCertificate.CertificateRequestId != certificateRequestId {
		withThumbprint := ""
		if renewReq.Thumbprint != "" {
			withThumbprint = fmt.Sprintf("with thumbprint %s ", renewReq.Thumbprint)
		}
		return "", fmt.Errorf(
			"certificate under requestId %s %s is not the latest under CertificateId %s."+
				"The latest request is %s. This error may happen when revoked certificate is requested to be renewed",
			certificateRequestId, withThumbprint, certificateId, managedCertificate.CertificateRequestId)
	}

	/* 4th step is to send renewal request */
	url := c.getURL(urlResourceCertificateRequests)
	if c.user == nil || c.user.Company == nil {
		return "", fmt.Errorf("must be autheticated to request a certificate")
	}

	req := certificateRequest{
		ExistingCertificateId: certificateId,
		ApplicationId:         applicationId,
		TemplateId:            templateId,
	}

	if renewReq.CertificateRequest.Location != nil {
		workload := renewReq.CertificateRequest.Location.Workload
		if workload == "" {
			workload = defaultAppName
		}
		nodeName := renewReq.CertificateRequest.Location.Instance
		appName := workload

		req.CertificateUsageMetadata = []certificateUsageMetadata{
			{
				AppName:  appName,
				NodeName: nodeName,
			},
		}
	}

	if renewReq.CertificateRequest != nil && len(renewReq.CertificateRequest.GetCSR()) != 0 {
		req.CSR = string(renewReq.CertificateRequest.GetCSR())
		req.ReuseCSR = false
	} else {
		req.ReuseCSR = true
		return "", fmt.Errorf("reuseCSR option is not currently available for Renew Certificate operation. A new CSR must be provided in the request")
	}
	statusCode, status, body, err := c.request("POST", url, req)
	if err != nil {
		return
	}

	cr, err := parseCertificateRequestResult(statusCode, status, body)
	if err != nil {
		return "", fmt.Errorf("failed to renew certificate: %s", err)
	}
	return cr.CertificateRequests[0].ID, nil
}

func (c *Connector) searchCertificates(req *SearchRequest) (*CertificateSearchResponse, error) {

	var err error

	url := c.getURL(urlResourceCertificateSearch)
	statusCode, _, body, err := c.request("POST", url, req)
	if err != nil {
		return nil, err
	}
	searchResult, err := ParseCertificateSearchResponse(statusCode, body)
	if err != nil {
		return nil, err
	}
	return searchResult, nil
}

func (c *Connector) searchCertificatesByFingerprint(fp string) (*CertificateSearchResponse, error) {
	fp = strings.Replace(fp, ":", "", -1)
	fp = strings.Replace(fp, ".", "", -1)
	fp = strings.ToUpper(fp)
	req := &SearchRequest{
		Expression: &Expression{
			Operands: []Operand{
				{
					"fingerprint",
					MATCH,
					fp,
				},
			},
		},
	}
	return c.searchCertificates(req)
}

/*
  "id": "32a656d1-69b1-11e8-93d8-71014a32ec53",
  "companyId": "b5ed6d60-22c4-11e7-ac27-035f0608fd2c",
  "latestCertificateRequestId": "0e546560-69b1-11e8-9102-a1f1c55d36fb",
  "ownerUserId": "593cdba0-2124-11e8-8219-0932652c1da0",
  "certificateIds": [
    "32a656d0-69b1-11e8-93d8-71014a32ec53"
  ],
  "certificateName": "cn=svc6.venafi.example.com",

*/
type managedCertificate struct {
	Id                   string `json:"id"`
	CompanyId            string `json:"companyId"`
	CertificateRequestId string `json:"certificateRequestId"`
}

func (c *Connector) getCertificate(certificateId string) (*managedCertificate, error) {
	var err error
	url := c.getURL(urlResourceCertificateByID)
	url = fmt.Sprintf(url, certificateId)
	statusCode, _, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}

	switch statusCode {
	case http.StatusOK:
		var res = &managedCertificate{}
		err = json.Unmarshal(body, res)
		if err != nil {
			return nil, fmt.Errorf("failed to parse search results: %s, body: %s", err, body)
		}
		return res, nil
	default:
		if body != nil {
			respErrors, err := parseResponseErrors(body)
			if err == nil {
				respError := fmt.Sprintf("unexpected status code on Venafi Cloud certificate search. Status: %d\n", statusCode)
				for _, e := range respErrors {
					respError += fmt.Sprintf("Error Code: %d Error: %s\n", e.Code, e.Message)
				}
				return nil, fmt.Errorf(respError)
			}
		}
		return nil, fmt.Errorf("unexpected status code on Venafi Cloud certificate search. Status: %d", statusCode)
	}
}

func (c *Connector) ImportCertificate(req *certificate.ImportRequest) (*certificate.ImportResponse, error) {
	pBlock, _ := pem.Decode([]byte(req.CertificateData))
	if pBlock == nil {
		return nil, fmt.Errorf("%w can`t parse certificate", verror.UserDataError)
	}
	zone := req.PolicyDN
	if zone == "" {
		appDetails, err := c.getAppDetailsByName(c.zone.getApplicationName())
		if err != nil {
			return nil, err
		}
		zone = appDetails.ApplicationId
	}
	ipAddr := endpoint.LocalIP
	origin := endpoint.SDKName
	for _, f := range req.CustomFields {
		if f.Type == certificate.CustomFieldOrigin {
			origin = f.Value
		}
	}
	base64.StdEncoding.EncodeToString(pBlock.Bytes)
	fingerprint := certThumbprint(pBlock.Bytes)
	request := importRequest{
		Certificates: []importRequestCertInfo{
			{
				Certificate:    base64.StdEncoding.EncodeToString(pBlock.Bytes),
				ApplicationIds: []string{zone},
				ApiClientInformation: apiClientInformation{
					Type:       origin,
					Identifier: ipAddr,
				},
			},
		},
	}

	url := c.getURL(urlResourceCertificates)
	statusCode, status, body, err := c.request("POST", url, request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", verror.ServerTemporaryUnavailableError, err)
	}
	var r importResponse
	switch statusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
	case http.StatusBadRequest, http.StatusForbidden, http.StatusConflict:
		return nil, fmt.Errorf("%w: certificate can`t be imported. %d %s %s", verror.ServerBadDataResponce, statusCode, status, string(body))
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return nil, verror.ServerTemporaryUnavailableError
	default:
		return nil, verror.ServerError
	}
	err = json.Unmarshal(body, &r)
	if err != nil {
		return nil, fmt.Errorf("%w: can`t unmarshal json response %s", verror.ServerError, err)
	} else if !(len(r.CertificateInformations) == 1) {
		return nil, fmt.Errorf("%w: certificate was not imported on unknown reason", verror.ServerBadDataResponce)
	}
	time.Sleep(time.Second)
	foundCert, err := c.searchCertificatesByFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	if len(foundCert.Certificates) != 1 {
		return nil, fmt.Errorf("%w certificate has been imported but could not be found on platform after that", verror.ServerError)
	}
	cert := foundCert.Certificates[0]
	resp := &certificate.ImportResponse{CertificateDN: cert.SubjectCN[0], CertId: cert.Id}
	return resp, nil
}

func (c *Connector) SetHTTPClient(client *http.Client) {
	c.client = client
}

func (c *Connector) ListCertificates(filter endpoint.Filter) ([]certificate.CertificateInfo, error) {
	if c.zone.String() == "" {
		return nil, fmt.Errorf("empty zone")
	}
	const batchSize = 50
	limit := 100000000
	if filter.Limit != nil {
		limit = *filter.Limit
	}
	var buf [][]certificate.CertificateInfo
	for page := 0; limit > 0; limit, page = limit-batchSize, page+1 {
		var b []certificate.CertificateInfo
		var err error
		b, err = c.getCertsBatch(page, batchSize, filter.WithExpired)
		if limit < batchSize && len(b) > limit {
			b = b[:limit]
		}
		if err != nil {
			return nil, err
		}
		buf = append(buf, b)
		if len(b) < batchSize {
			break
		}
	}
	sumLen := 0
	for _, b := range buf {
		sumLen += len(b)
	}
	infos := make([]certificate.CertificateInfo, sumLen)
	offset := 0
	for _, b := range buf {
		copy(infos[offset:], b[:])
		offset += len(b)
	}
	return infos, nil
}

func (c *Connector) getCertsBatch(page, pageSize int, withExpired bool) ([]certificate.CertificateInfo, error) {

	appDetails, err := c.getAppDetailsByName(c.zone.getApplicationName())
	if err != nil {
		return nil, err
	}

	req := &SearchRequest{
		Expression: &Expression{
			Operands: []Operand{
				{"appstackIds", MATCH, appDetails.ApplicationId},
			},
			Operator: AND,
		},
		Paging: &Paging{PageSize: pageSize, PageNumber: page},
	}
	if !withExpired {
		req.Expression.Operands = append(req.Expression.Operands, Operand{
			"validityEnd",
			GTE,
			time.Now().Format(time.RFC3339),
		})
	}
	r, err := c.searchCertificates(req)
	if err != nil {
		return nil, err
	}
	infos := make([]certificate.CertificateInfo, len(r.Certificates))
	for i, c := range r.Certificates {
		infos[i] = c.ToCertificateInfo()
	}
	return infos, nil
}

func (c *Connector) getAppDetailsByName(appName string) (*ApplicationDetails, error) {
	url := c.getURL(urlAppDetailsByName)
	if c.user == nil {
		return nil, fmt.Errorf("must be autheticated to read the zone configuration")
	}
	encodedAppName := netUrl.PathEscape(appName)
	url = fmt.Sprintf(url, encodedAppName)
	statusCode, status, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	details, err := parseApplicationDetailsResult(statusCode, status, body)
	if err != nil {
		return nil, err
	}
	return details, nil
}

func (c *Connector) getTemplateByID() (*certificateTemplate, error) {
	url := c.getURL(urlResourceTemplate)
	appNameEncoded := netUrl.PathEscape(c.zone.getApplicationName())
	citAliasEncoded := netUrl.PathEscape(c.zone.getTemplateAlias())
	url = fmt.Sprintf(url, appNameEncoded, citAliasEncoded)
	statusCode, status, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	t, err := parseCertificateTemplateResult(statusCode, status, body)
	return t, err
}
