package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alphagov/paas-aiven-broker/provider/aiven"
	"github.com/pivotal-cf/brokerapi"
)

const AIVEN_BASE_URL string = "https://api.aiven.io"
const SERVICE_TYPE string = "elasticsearch"

type AivenProvider struct {
	Client aiven.Client
	Config *Config
}

func New(configJSON []byte) (*AivenProvider, error) {
	config, err := DecodeConfig(configJSON)
	if err != nil {
		return nil, err
	}
	client := aiven.NewHttpClient(AIVEN_BASE_URL, config.APIToken, config.Project)
	return &AivenProvider{
		Client: client,
		Config: config,
	}, nil
}

func (ap *AivenProvider) Provision(ctx context.Context, provisionData ProvisionData) (dashboardURL, operationData string, err error) {
	plan, err := ap.Config.FindPlan(provisionData.Service.ID, provisionData.Plan.ID)
	if err != nil {
		return "", "", err
	}
	ipFilter, err := ParseIPWhitelist(os.Getenv("IP_WHITELIST"))
	if err != nil {
		return "", "", err
	}
	createServiceInput := &aiven.CreateServiceInput{
		Cloud:       ap.Config.Cloud,
		Plan:        plan.AivenPlan,
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, provisionData.InstanceID),
		ServiceType: SERVICE_TYPE,
		UserConfig: aiven.UserConfig{
			ElasticsearchVersion: plan.ElasticsearchVersion,
			IPFilter:             ipFilter,
		},
	}
	_, err = ap.Client.CreateService(createServiceInput)
	return dashboardURL, operationData, err
}

func (ap *AivenProvider) Deprovision(ctx context.Context, deprovisionData DeprovisionData) (operationData string, err error) {
	err = ap.Client.DeleteService(&aiven.DeleteServiceInput{
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, deprovisionData.InstanceID),
	})

	if err != nil {
		if err == aiven.ErrInstanceDoesNotExist {
			return "", brokerapi.ErrInstanceDoesNotExist
		}
	}

	return "", err
}

type Credentials struct {
	URI      string `json:"uri"`
	Hostname string `json:"hostname"`
	Port     string `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (ap *AivenProvider) Bind(ctx context.Context, bindData BindData) (binding brokerapi.Binding, err error) {
	user := bindData.BindingID
	password, err := ap.Client.CreateServiceUser(&aiven.CreateServiceUserInput{
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, bindData.InstanceID),
		Username:    user,
	})
	if err != nil {
		return brokerapi.Binding{}, err
	}

	host, port, err := ap.Client.GetServiceConnectionDetails(&aiven.GetServiceInput{
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, bindData.InstanceID),
	})
	if err != nil {
		return brokerapi.Binding{}, err
	}

	credentials := Credentials{
		URI:      buildURI(user, password, host, port),
		Hostname: host,
		Port:     port,
		Username: user,
		Password: password,
	}

	return brokerapi.Binding{
		Credentials: credentials,
	}, nil
}

func buildURI(user, password, host, port string) string {
	uri := &url.URL{
		Scheme: "https",
		User:   url.UserPassword(user, password),
		Host:   fmt.Sprintf("%s:%s", host, port),
	}
	return uri.String()
}

func (ap *AivenProvider) Unbind(ctx context.Context, unbindData UnbindData) (err error) {
	_, err = ap.Client.DeleteServiceUser(&aiven.DeleteServiceUserInput{
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, unbindData.InstanceID),
		Username:    unbindData.BindingID,
	})
	return err
}

func (ap *AivenProvider) Update(ctx context.Context, updateData UpdateData) (operationData string, err error) {
	plan, err := ap.Config.FindPlan(updateData.Details.ServiceID, updateData.Details.PlanID)
	if err != nil {
		return "", err
	}

	ipFilter, err := ParseIPWhitelist(os.Getenv("IP_WHITELIST"))
	if err != nil {
		return "", err
	}

	_, err = ap.Client.UpdateService(&aiven.UpdateServiceInput{
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, updateData.InstanceID),
		Plan:        plan.AivenPlan,
		UserConfig: aiven.UserConfig{
			ElasticsearchVersion: plan.ElasticsearchVersion,
			IPFilter:             ipFilter,
		},
	})

	switch err := err.(type) {
	case aiven.ErrInvalidUpdate:
		return "", brokerapi.NewFailureResponseBuilder(
			err,
			http.StatusUnprocessableEntity,
			"plan-change-not-supported",
		).WithErrorKey("PlanChangeNotSupported").Build()
	default:
		return "", err
	}

	return "", nil
}

func (ap *AivenProvider) LastOperation(ctx context.Context, lastOperationData LastOperationData) (state brokerapi.LastOperationState, description string, err error) {
	status, updateTime, err := ap.Client.GetServiceStatus(&aiven.GetServiceInput{
		ServiceName: buildServiceName(ap.Config.ServiceNamePrefix, lastOperationData.InstanceID),
	})
	if err != nil {
		return "", "", err
	}

	if updateTime.After(time.Now().Add(-1 * 60 * time.Second)) {
		return brokerapi.InProgress, "Preparing to apply update", nil
	}

	lastOperationState, description := providerStatesMapping(status)
	return lastOperationState, description, nil
}

func ParseIPWhitelist(ips string) ([]string, error) {
	if ips == "" {
		return []string{}, nil
	}
	outIPs := []string{}
	for _, ip := range strings.Split(ips, ",") {
		if len(strings.Split(ip, ".")) != 4 {
			return []string{}, fmt.Errorf("malformed whitelist IP: %v", ip)
		}
		outIPs = append(outIPs, ip)
	}
	return outIPs, nil
}

func buildServiceName(prefix, guid string) string {
	return strings.ToLower(prefix + "-" + guid)
}

func providerStatesMapping(status aiven.ServiceStatus) (brokerapi.LastOperationState, string) {
	switch status {
	case aiven.Running:
		return brokerapi.Succeeded, "Last operation succeeded"
	case aiven.Rebuilding:
		return brokerapi.InProgress, "Rebuilding"
	case aiven.Rebalancing:
		return brokerapi.InProgress, "Rebalancing"
	case aiven.PowerOff:
		return brokerapi.Failed, "Last operation failed: service is powered off"
	default:
		return brokerapi.InProgress, fmt.Sprintf("Unknown state: %s", status)
	}
}
