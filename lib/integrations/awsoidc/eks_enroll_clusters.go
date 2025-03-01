/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package awsoidc

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	eksTypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	helmCli "helm.sh/helm/v3/pkg/cli"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"

	"github.com/gravitational/teleport/api/types"
	apiutils "github.com/gravitational/teleport/api/utils"
	awslib "github.com/gravitational/teleport/lib/cloud/aws"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/discovery/common"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	// https://docs.aws.amazon.com/eks/latest/userguide/access-policies.html
	// We use cluster admin policy to create namespace and cluster role.
	eksClusterAdminPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

	agentRepoURL                = "https://charts.releases.teleport.dev"
	agentNamespace              = "teleport-agent"
	agentName                   = "teleport-kube-agent"
	awsKubePrefix               = "k8s-aws-v1."
	awsHeaderClusterName        = "x-k8s-aws-id"
	concurrentEKSEnrollingLimit = 5
)

// EnrollEKSClusterResult contains result for a single EKS cluster enrollment, if it was successful 'Error' will be nil
// otherwise it will contain an error happened during enrollment.
type EnrollEKSClusterResult struct {
	// ClusterName is the name of an EKS cluster.
	ClusterName string
	// ResourceId is resource ID for the cluster, it is taken from the join token used to enroll the cluster.
	ResourceId string
	// Error contains an error that happened during enrollment, if there was one.
	Error error
}

// EnrollEKSClusterResponse contains result for enrollment .
type EnrollEKSClusterResponse struct {
	// Results contain an error per a cluster enrollment if there was one.
	Results []EnrollEKSClusterResult
}

// EnrollEKSCLusterClient defines functions required for EKS cluster enrollment.
type EnrollEKSCLusterClient interface {
	// CreateAccessEntry creates an access entry. An access entry allows an IAM principal to access an EKS cluster.
	CreateAccessEntry(ctx context.Context, params *eks.CreateAccessEntryInput, optFns ...func(*eks.Options)) (*eks.CreateAccessEntryOutput, error)

	// AssociateAccessPolicy associates an access policy and its scope to an access entry.
	AssociateAccessPolicy(ctx context.Context, params *eks.AssociateAccessPolicyInput, optFns ...func(*eks.Options)) (*eks.AssociateAccessPolicyOutput, error)

	// ListAccessEntries lists the access entries for an EKS cluster.
	ListAccessEntries(ctx context.Context, params *eks.ListAccessEntriesInput, optFns ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error)

	// DeleteAccessEntry deletes an access entry from an EKS cluster.
	DeleteAccessEntry(ctx context.Context, params *eks.DeleteAccessEntryInput, optFns ...func(*eks.Options)) (*eks.DeleteAccessEntryOutput, error)

	// DescribeCluster returns detailed information about an EKS cluster.
	DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)

	// GetCallerIdentity returns details about the IAM user or role whose credentials are used to call the operation.
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)

	// CheckAgentAlreadyInstalled checks if teleport-kube-agent Helm chart is already installed on the EKS cluster.
	CheckAgentAlreadyInstalled(genericclioptions.RESTClientGetter, logrus.FieldLogger) (bool, error)

	// InstallKubeAgent installs teleport-kube-agent Helm chart to the EKS cluster.
	InstallKubeAgent(context.Context, *eksTypes.Cluster, string, string, string, genericclioptions.RESTClientGetter, logrus.FieldLogger, EnrollEKSClustersRequest) error

	// CreateToken creates provisioning token on the auth server. That token can be used to install kube agent to an EKS cluster.
	CreateToken(context.Context, types.ProvisionToken) error
}

type defaultEnrollEKSClustersClient struct {
	*eks.Client
	stsClient    *sts.Client
	tokenCreator TokenCreator
}

// GetCallerIdentity returns details about the IAM user or role whose credentials are used to call the operation.
func (d *defaultEnrollEKSClustersClient) GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return d.stsClient.GetCallerIdentity(ctx, params, optFns...)
}

// CheckAgentAlreadyInstalled checks if teleport-kube-agent Helm chart is already installed on the EKS cluster.
func (d *defaultEnrollEKSClustersClient) CheckAgentAlreadyInstalled(clientGetter genericclioptions.RESTClientGetter, log logrus.FieldLogger) (bool, error) {
	actionConfig, err := getHelmActionConfig(clientGetter, log)
	if err != nil {
		return false, trace.Wrap(err)
	}

	return checkAgentAlreadyInstalled(actionConfig)
}

func getToken(ctx context.Context, clock clockwork.Clock, tokenCreator TokenCreator) (string, string, error) {
	const eksJoinTokenTTL = 30 * time.Minute

	tokenName, err := utils.CryptoRandomHex(defaults.TokenLenBytes)
	if err != nil {
		return "", "", trace.Wrap(err)
	}
	expires := clock.Now().UTC().Add(eksJoinTokenTTL)

	resourceId := uuid.NewString()
	req := types.ProvisionTokenSpecV2{
		SuggestedLabels: types.Labels{
			types.InternalResourceIDLabel: apiutils.Strings{resourceId},
		},
		Roles: []types.SystemRole{types.RoleKube, types.RoleApp, types.RoleDiscovery},
	}

	provisionToken, err := types.NewProvisionTokenFromSpec(tokenName, expires, req)
	if err != nil {
		return "", "", trace.Wrap(err)
	}

	err = tokenCreator(ctx, provisionToken)
	if err != nil {
		return "", "", trace.Wrap(err)
	}

	return provisionToken.GetName(), resourceId, trace.Wrap(err)
}

// InstallKubeAgent installs teleport-kube-agent Helm chart to the EKS cluster.
func (d *defaultEnrollEKSClustersClient) InstallKubeAgent(ctx context.Context, eksCluster *eksTypes.Cluster, proxyAddr, joinToken, resourceId string, clientGetter genericclioptions.RESTClientGetter, log logrus.FieldLogger, req EnrollEKSClustersRequest) error {
	actionConfig, err := getHelmActionConfig(clientGetter, log)
	if err != nil {
		return trace.Wrap(err)
	}
	settings, err := getHelmSettings()
	if err != nil {
		return trace.Wrap(err)
	}

	return installKubeAgent(ctx, installKubeAgentParams{
		eksCluster:   eksCluster,
		proxyAddr:    proxyAddr,
		joinToken:    joinToken,
		resourceID:   resourceId,
		actionConfig: actionConfig,
		settings:     settings,
		req:          req,
		log:          log,
	})
}

// CreateToken creates provisioning token on the auth server. That token can be used to install kube agent to an EKS cluster.
func (d *defaultEnrollEKSClustersClient) CreateToken(ctx context.Context, token types.ProvisionToken) error {
	return d.tokenCreator(ctx, token)
}

// TokenCreator creates join token on the auth server.
type TokenCreator func(ctx context.Context, token types.ProvisionToken) error

// NewEnrollEKSClustersClient returns new client that can be used to enroll EKS clusters into Teleport.
func NewEnrollEKSClustersClient(ctx context.Context, req *AWSClientRequest, tokenCreator TokenCreator) (EnrollEKSCLusterClient, error) {
	eksClient, err := newEKSClient(ctx, req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	stsClient, err := newSTSClient(ctx, req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clt := defaultEnrollEKSClustersClient{
		Client:       eksClient,
		stsClient:    stsClient,
		tokenCreator: tokenCreator,
	}

	return &clt, nil
}

// EnrollEKSClustersRequest contains the required fields to enroll EKS cluster to Teleport.
type EnrollEKSClustersRequest struct {
	// Region is the AWS Region.
	Region string

	// ClusterNames is name of the EKS cluster to enroll.
	ClusterNames []string

	// EnableAppDiscovery specifies if we should enable Kubernetes App Discovery inside the enrolled EKS cluster.
	EnableAppDiscovery bool

	// EnableAutoUpgrades specifies if we should enable agent auto upgrades.
	EnableAutoUpgrades bool

	// IsCloud specifies if enrollment is done for the Teleport Cloud client.
	IsCloud bool

	// AgentVersion specifies version of the Helm chart that will be installed during enrollment.
	AgentVersion string
}

// EnrollEKSClusters enrolls EKS clusters into Teleport by installing teleport-kube-agent chart on the clusters.
// It returns list of result individually for each EKS cluster. Clusters are enrolled concurrently. If an error occurs
// during a cluster enrollment an error message will be present in the result for this cluster. Otherwise result will
// contain resource ID - this is ID from the join token used by the enrolled cluster and can be used by UI to check
// when agent joins Teleport cluster.
//
// During enrollment we create access entry for an EKS cluster if needed and cluster admin policy is associated with that entry,
// so our AWS integration can access the target EKS cluster during the chart installation. After enrollment is done we remove
// the access entry (if it was created by us), since we don't need it anymore.
func EnrollEKSClusters(ctx context.Context, log logrus.FieldLogger, clock clockwork.Clock, proxyAddr string, credsProvider aws.CredentialsProvider, clt EnrollEKSCLusterClient, req EnrollEKSClustersRequest) *EnrollEKSClusterResponse {
	var mu sync.Mutex
	var results []EnrollEKSClusterResult

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(concurrentEKSEnrollingLimit)

	for _, eksClusterName := range req.ClusterNames {
		eksClusterName := eksClusterName

		group.Go(func() error {
			resourceId, err := enrollEKSCluster(ctx, log, clock, credsProvider, clt, proxyAddr, eksClusterName, req)
			if err != nil {
				log.WithError(err).Debugf("failed to enroll EKS cluster %q", eksClusterName)
			}

			mu.Lock()
			defer mu.Unlock()
			results = append(results, EnrollEKSClusterResult{ClusterName: eksClusterName, ResourceId: resourceId, Error: trace.Wrap(err)})

			return nil
		})
	}
	// We don't return error from individual group goroutines, they are gathered in the returned value.
	_ = group.Wait()

	return &EnrollEKSClusterResponse{Results: results}
}

func enrollEKSCluster(ctx context.Context, log logrus.FieldLogger, clock clockwork.Clock, credsProvider aws.CredentialsProvider, clt EnrollEKSCLusterClient, proxyAddr, clusterName string, req EnrollEKSClustersRequest) (string, error) {
	eksClusterInfo, err := clt.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(clusterName),
	})
	if err != nil {
		return "", trace.Wrap(err, "unable to describe EKS cluster")
	}
	eksCluster := eksClusterInfo.Cluster

	if eksCluster.Status != eksTypes.ClusterStatusActive {
		return "", trace.BadParameter(`can't enroll EKS cluster %q - expected "ACTIVE" state, got %q.`, clusterName, eksCluster.Status)
	}

	principalArn, err := getAccessEntryPrincipalArn(ctx, clt.GetCallerIdentity)
	if err != nil {
		return "", trace.Wrap(err)
	}

	wasAdded, err := maybeAddAccessEntry(ctx, clusterName, principalArn, clt)
	if err != nil {
		return "", trace.Wrap(err)
	}
	if wasAdded {
		// If we added access entry, we'll clean it up when function stops executing.
		defer func() {
			if _, err := clt.DeleteAccessEntry(ctx, &eks.DeleteAccessEntryInput{
				ClusterName:  aws.String(clusterName),
				PrincipalArn: aws.String(principalArn),
			}); err != nil {
				log.WithError(err).Warnf("could not delete access entry for principal %q on cluster %q", principalArn, clusterName)
			}
		}()
	}

	_, err = clt.AssociateAccessPolicy(ctx, &eks.AssociateAccessPolicyInput{
		AccessScope: &eksTypes.AccessScope{
			Namespaces: nil,
			Type:       eksTypes.AccessScopeTypeCluster,
		},
		ClusterName:  aws.String(clusterName),
		PolicyArn:    aws.String(eksClusterAdminPolicy),
		PrincipalArn: aws.String(principalArn),
	})
	if err != nil {
		return "", trace.Wrap(err, "unable to associate EKS Access Policy to cluster %q", clusterName)
	}

	kubeClientGetter, err := getKubeClientGetter(ctx, clock.Now(), credsProvider, clusterName, req.Region,
		aws.ToString(eksCluster.CertificateAuthority.Data), aws.ToString(eksCluster.Endpoint))
	if err != nil {
		return "", trace.Wrap(err, "unable to build kubernetes client for EKS cluster %q", clusterName)
	}

	if alreadyInstalled, err := clt.CheckAgentAlreadyInstalled(kubeClientGetter, log); err != nil {
		return "", trace.Wrap(err, "could not check if teleport-kube-agent is already installed.")
	} else if alreadyInstalled {
		return "", trace.AlreadyExists("teleport-kube-agent is already installed on the cluster %q", clusterName)
	}

	joinToken, resourceId, err := getToken(ctx, clock, clt.CreateToken)
	if err != nil {
		return "", trace.Wrap(err)
	}

	if err := clt.InstallKubeAgent(ctx, eksCluster, proxyAddr, joinToken, resourceId, kubeClientGetter, log, req); err != nil {
		return "", trace.Wrap(err)
	}

	return resourceId, nil
}

// IdentityGetter returns AWS identity of the caller.
type IdentityGetter func(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)

func getAccessEntryPrincipalArn(ctx context.Context, identityGetter IdentityGetter) (string, error) {
	ident, err := identityGetter(ctx, nil)
	if err != nil {
		return "", trace.Wrap(err)
	}

	parsedIdentity, err := awslib.IdentityFromArn(aws.ToString(ident.Arn))
	if err != nil {
		return "", trace.Wrap(err)
	}

	return fmt.Sprintf("arn:aws:iam::%s:role/%s", parsedIdentity.GetAccountID(), parsedIdentity.GetName()), nil
}

// maybeAddAccessEntry checks list of access entries for the EKS cluster and adds one for Teleport if it's missing.
// If access entry was added by this function it will return true as a first value.
func maybeAddAccessEntry(ctx context.Context, clusterName, roleArn string, clt EnrollEKSCLusterClient) (bool, error) {
	entries, err := clt.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{
		ClusterName: aws.String(clusterName),
	})
	if err != nil {
		return false, trace.Wrap(err)
	}

	for _, entry := range entries.AccessEntries {
		if entry == roleArn {
			return false, nil
		}
	}

	_, err = clt.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{
		ClusterName:  aws.String(clusterName),
		PrincipalArn: aws.String(roleArn),
	})
	return err == nil, trace.Wrap(err)
}

// getPresignURL returns a specially formatted URL that can be presigned and used in EKS authentication.
func getPresignURL() url.URL {
	endpoint := "sts.amazonaws.com"
	q := url.Values{}
	q.Set("Action", "GetCallerIdentity")
	q.Set("Version", "2011-06-15")
	q.Set("X-Amz-Expires", "60")

	return url.URL{
		Scheme:   "https",
		Host:     endpoint,
		Path:     "/",
		RawQuery: q.Encode(),
	}
}

// getKubeClientGetter returns client getter for kube that can be used to access target EKS cluster
func getKubeClientGetter(ctx context.Context, timestamp time.Time, credsProvider aws.CredentialsProvider, clusterName, region, clusterCA, clusterEndpoint string) (*genericclioptions.ConfigFlags, error) {
	targetUrl := getPresignURL()

	r, err := http.NewRequest(http.MethodGet, targetUrl.String(), nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	r.Header.Add(awsHeaderClusterName, clusterName)
	creds, err := credsProvider.Retrieve(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	signer := v4.NewSigner()
	presigned, _, err := signer.PresignHTTP(ctx, creds, r, hashForGetRequests, "sts", region, timestamp)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	kubeToken := awsKubePrefix + base64.RawURLEncoding.EncodeToString([]byte(presigned))

	eksClusterCA, err := base64.StdEncoding.DecodeString(clusterCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	restConfig := &rest.Config{
		Host:        clusterEndpoint,
		BearerToken: kubeToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: eksClusterCA,
		},
	}

	configFlags := genericclioptions.NewConfigFlags(false)
	configFlags.WithWrapConfigFn(func(*rest.Config) *rest.Config {
		return restConfig
	})

	return configFlags, nil
}

func getHelmActionConfig(clientGetter genericclioptions.RESTClientGetter, log logrus.FieldLogger) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(clientGetter, agentNamespace, "secret", log.WithField("source", "helm").Debugf); err != nil {
		return nil, trace.Wrap(err)
	}

	return actionConfig, nil
}

func getHelmSettings() (*helmCli.EnvSettings, error) {
	helmSettings := helmCli.New()
	dir, err := os.MkdirTemp(os.TempDir(), "teleport-eks-chart-")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	helmSettings.RepositoryCache = dir
	helmSettings.SetNamespace(agentNamespace)

	return helmSettings, nil
}

// checkAgentAlreadyInstalled checks through the Helm if teleport-kube-agent chart was already installed in the EKS cluster.
func checkAgentAlreadyInstalled(actionConfig *action.Configuration) (bool, error) {
	listCmd := action.NewList(actionConfig)

	releases, err := listCmd.Run()
	if err != nil {
		return false, trace.Wrap(err)
	}
	for _, release := range releases {
		if release.Name == agentName {
			return true, nil
		}
	}
	return false, nil
}

type installKubeAgentParams struct {
	eksCluster   *eksTypes.Cluster
	proxyAddr    string
	joinToken    string
	resourceID   string
	actionConfig *action.Configuration
	settings     *helmCli.EnvSettings
	req          EnrollEKSClustersRequest
	log          logrus.FieldLogger
}

// installKubeAgent installs teleport-kube-agent chart to the target EKS cluster.
func installKubeAgent(ctx context.Context, cfg installKubeAgentParams) error {
	defer func() {
		// Clean up temporary chart cache directory.
		err := os.RemoveAll(cfg.settings.RepositoryCache)
		if err != nil && cfg.log != nil {
			cfg.log.Warnf("could not delete temporary chart cache directory at the path %q", cfg.settings.RepositoryCache)
		}
	}()

	installCmd := action.NewInstall(cfg.actionConfig)
	installCmd.RepoURL = agentRepoURL
	installCmd.Version = cfg.req.AgentVersion
	if strings.Contains(installCmd.Version, "dev") {
		installCmd.Version = "" // For testing during development.
	}

	chartPath, err := installCmd.LocateChart(agentName, cfg.settings)
	if err != nil {
		return trace.Wrap(err, "could not locate chart")
	}

	agentChart, err := loader.Load(chartPath)
	if err != nil {
		return trace.Wrap(err)
	}
	installCmd.ReleaseName = agentName
	installCmd.Namespace = agentNamespace
	installCmd.CreateNamespace = true
	vals := map[string]any{}
	vals["proxyAddr"] = cfg.proxyAddr

	vals["roles"] = "kube"
	// todo(anton): Remove check for 13 once Teleport cloud is unblocked to move from v13 chart.
	if cfg.req.EnableAppDiscovery && !strings.HasPrefix(installCmd.Version, "13") {
		vals["roles"] = "kube,app,discovery"
	}
	vals["authToken"] = cfg.joinToken

	if cfg.req.IsCloud && cfg.req.EnableAutoUpgrades {
		vals["updater"] = map[string]any{"enabled": true, "releaseChannel": "stable/cloud"}

		vals["highAvailability"] = map[string]any{"replicaCount": 2,
			"podDisruptionBudget": map[string]any{"enabled": true, "minAvailable": 1},
		}
	}

	eksTags := make(map[string]*string, len(cfg.eksCluster.Tags))
	for k, v := range cfg.eksCluster.Tags {
		eksTags[k] = aws.String(v)
	}
	kubeCluster, err := services.NewKubeClusterFromAWSEKS(aws.ToString(cfg.eksCluster.Name), aws.ToString(cfg.eksCluster.Arn), eksTags)
	if err != nil {
		return trace.Wrap(err)
	}
	common.ApplyEKSNameSuffix(kubeCluster)
	vals["kubeClusterName"] = kubeCluster.GetName()

	labels := kubeCluster.GetStaticLabels()
	labels[types.InternalResourceIDLabel] = cfg.resourceID
	vals["labels"] = labels

	if _, err := installCmd.RunWithContext(ctx, agentChart, vals); err != nil {
		return trace.Wrap(err, "could not install Helm chart.")
	}

	return nil
}
