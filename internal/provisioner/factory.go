/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provisioner

import (
	"context"
	"fmt"
	"os"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/aruba"
	"github.com/diegobraga/cloudburst-autoscaler/internal/aws"
	"github.com/diegobraga/cloudburst-autoscaler/internal/azure"
	"github.com/diegobraga/cloudburst-autoscaler/internal/cherry_servers"
	"github.com/diegobraga/cloudburst-autoscaler/internal/clouding"
	"github.com/diegobraga/cloudburst-autoscaler/internal/contabo"
	"github.com/diegobraga/cloudburst-autoscaler/internal/digitalocean"
	"github.com/diegobraga/cloudburst-autoscaler/internal/exoscale"
	"github.com/diegobraga/cloudburst-autoscaler/internal/gcore"
	"github.com/diegobraga/cloudburst-autoscaler/internal/gcp"
	"github.com/diegobraga/cloudburst-autoscaler/internal/hetzner"
	"github.com/diegobraga/cloudburst-autoscaler/internal/ibmcloud"
	"github.com/diegobraga/cloudburst-autoscaler/internal/ovh"
	"github.com/diegobraga/cloudburst-autoscaler/internal/scaleway"
	"github.com/diegobraga/cloudburst-autoscaler/internal/seeweb"
	"github.com/diegobraga/cloudburst-autoscaler/internal/upcloud"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Getter can read objects from the API server (e.g. secrets). *NodeClaimReconciler satisfies this.
type Getter interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

// NewFromNodeClass builds the correct Provisioner for the given provider,
// using region from the CloudBroker recommendation and credentials from NodeClass.
// Caller must call Close() on the returned Provisioner when done.
func NewFromNodeClass(ctx context.Context, provider string, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if region == "" {
		return nil, fmt.Errorf("region is required (from NodeClaim.Spec.Chosen.Region)")
	}
	switch provider {
	case "gcp":
		return newGCP(ctx, nodeClass, region)
	case "scaleway":
		return newScaleway(ctx, nodeClass, namespace, c, region)
	case "hetzner":
		return newHetzner(ctx, nodeClass, namespace, c, region)
	case "digitalocean":
		return newDigitalOcean(ctx, nodeClass, namespace, c, region)
	case "ovh":
		return newOVH(ctx, nodeClass, namespace, c, region)
	case "aws":
		return newAWS(ctx, nodeClass, namespace, c, region)
	case "azure":
		return newAzure(ctx, nodeClass, namespace, c, region)
	case "aruba":
		return newAruba(ctx, nodeClass, namespace, c, region)
	case "upcloud":
		return newUpCloud(ctx, nodeClass, namespace, c, region)
	case "exoscale":
		return newExoscale(ctx, nodeClass, namespace, c, region)
	case "seeweb":
		return newSeeweb(ctx, nodeClass, namespace, c, region)
	case "contabo":
		return newContabo(ctx, nodeClass, namespace, c, region)
	case "gcore":
		return newGcore(ctx, nodeClass, namespace, c, region)
	case "cherry_servers":
		return newCherryServers(ctx, nodeClass, namespace, c, region)
	case "clouding":
		return newClouding(ctx, nodeClass, namespace, c, region)
	case "ibm_cloud":
		return newIBMCloud(ctx, nodeClass, namespace, c, region)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
}

func newGCP(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, regionOrZone string) (Provisioner, error) {
	project := gcp.GetProject(nodeClass.Spec.GCP.Project)
	if project == "" {
		return nil, fmt.Errorf("project must be set in NodeClass.Spec.GCP")
	}
	// regionOrZone is already the zone (from getProvisioningParams, which appends suffix for GCP)
	return gcp.NewProvisioner(ctx, project, regionOrZone, nodeClass.Spec.GCP.ImageFamily, nodeClass.Spec.GCP.ImageProject)
}

func newScaleway(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, zone string) (Provisioner, error) {
	if nodeClass.Spec.Scaleway == nil {
		return nil, fmt.Errorf("NodeClass spec.scaleway is required for Scaleway provider")
	}
	var apiSecretKey, apiAccessKey string
	if nodeClass.Spec.Scaleway.APIKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Scaleway.APIKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Scaleway API key secret: %w", err)
		}
		k := nodeClass.Spec.Scaleway.APIKeySecretRef.Key
		if k == "" {
			k = "SCW_SECRET_KEY"
		}
		apiSecretKey = string(secret.Data[k])
		if apiSecretKey == "" {
			apiSecretKey = string(secret.Data["secret-key"])
		}
		if apiSecretKey == "" {
			return nil, fmt.Errorf("Scaleway API key secret missing key %s or 'secret-key'", k)
		}
		apiAccessKey = string(secret.Data["SCW_ACCESS_KEY"])
		if apiAccessKey == "" {
			apiAccessKey = string(secret.Data["access-key"])
		}
	}
	if apiAccessKey == "" {
		apiAccessKey = os.Getenv("SCW_ACCESS_KEY")
	}
	if apiSecretKey == "" {
		apiSecretKey = os.Getenv("SCW_SECRET_KEY")
	}
	return scaleway.NewProvisioner(ctx, zone, nodeClass.Spec.Scaleway.ProjectID, nodeClass.Spec.Scaleway.OrganizationID, apiAccessKey, apiSecretKey)
}

func newHetzner(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, location string) (Provisioner, error) {
	if nodeClass.Spec.Hetzner == nil {
		return nil, fmt.Errorf("NodeClass spec.hetzner is required for Hetzner provider")
	}
	var apiToken string
	if nodeClass.Spec.Hetzner.APITokenSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Hetzner.APITokenSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Hetzner API token secret: %w", err)
		}
		k := nodeClass.Spec.Hetzner.APITokenSecretRef.Key
		if k == "" {
			k = "HETZNER_API_TOKEN"
		}
		apiToken = string(secret.Data[k])
		if apiToken == "" {
			return nil, fmt.Errorf("Hetzner API token secret missing key %s", k)
		}
	} else {
		apiToken = os.Getenv("HETZNER_API_TOKEN")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("no Hetzner API token: set spec.hetzner.apiTokenSecretRef or HETZNER_API_TOKEN env var")
	}
	return hetzner.NewProvisioner(ctx, apiToken, location)
}

func newDigitalOcean(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.DigitalOcean == nil {
		return nil, fmt.Errorf("NodeClass spec.digitalocean is required for DigitalOcean provider")
	}
	var apiToken string
	if nodeClass.Spec.DigitalOcean.APITokenSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.DigitalOcean.APITokenSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get DigitalOcean API token secret: %w", err)
		}
		k := nodeClass.Spec.DigitalOcean.APITokenSecretRef.Key
		if k == "" {
			k = "DIGITALOCEAN_API_TOKEN"
		}
		apiToken = string(secret.Data[k])
		if apiToken == "" {
			return nil, fmt.Errorf("DigitalOcean API token secret missing key %s", k)
		}
	} else {
		apiToken = os.Getenv("DIGITALOCEAN_API_TOKEN")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("no DigitalOcean API token: set spec.digitalocean.apiTokenSecretRef or DIGITALOCEAN_API_TOKEN env var")
	}
	return digitalocean.NewProvisioner(ctx, apiToken, region)
}

func newOVH(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.OVH == nil || nodeClass.Spec.OVH.ProjectID == "" {
		return nil, fmt.Errorf("NodeClass spec.ovh with projectID is required for OVH provider")
	}
	projectID := nodeClass.Spec.OVH.ProjectID
	var appKey, appSecret, consumerKey string
	if nodeClass.Spec.OVH.ApplicationKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.OVH.ApplicationKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get OVH application key secret: %w", err)
		}
		k := nodeClass.Spec.OVH.ApplicationKeySecretRef.Key
		if k == "" {
			k = "OVH_APPLICATION_KEY"
		}
		appKey = string(secret.Data[k])
		if appKey == "" {
			appKey = string(secret.Data["applicationKey"])
		}
	}
	if nodeClass.Spec.OVH.ApplicationSecretSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.OVH.ApplicationSecretSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get OVH application secret: %w", err)
		}
		k := nodeClass.Spec.OVH.ApplicationSecretSecretRef.Key
		if k == "" {
			k = "OVH_APPLICATION_SECRET"
		}
		appSecret = string(secret.Data[k])
		if appSecret == "" {
			appSecret = string(secret.Data["applicationSecret"])
		}
	}
	if nodeClass.Spec.OVH.ConsumerKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.OVH.ConsumerKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get OVH consumer key secret: %w", err)
		}
		k := nodeClass.Spec.OVH.ConsumerKeySecretRef.Key
		if k == "" {
			k = "OVH_CONSUMER_KEY"
		}
		consumerKey = string(secret.Data[k])
		if consumerKey == "" {
			consumerKey = string(secret.Data["consumerKey"])
		}
	}
	if appKey == "" {
		appKey = os.Getenv("OVH_APPLICATION_KEY")
	}
	if appSecret == "" {
		appSecret = os.Getenv("OVH_APPLICATION_SECRET")
	}
	if consumerKey == "" {
		consumerKey = os.Getenv("OVH_CONSUMER_KEY")
	}
	if appKey == "" || appSecret == "" || consumerKey == "" {
		return nil, fmt.Errorf("OVH credentials required: set spec.ovh secret refs or OVH_APPLICATION_KEY, OVH_APPLICATION_SECRET, OVH_CONSUMER_KEY env vars")
	}
	endpoint := nodeClass.Spec.OVH.Endpoint
	if endpoint == "" {
		endpoint = "ovh-eu"
	}
	return ovh.NewProvisioner(ctx, endpoint, appKey, appSecret, consumerKey, projectID, region)
}

func newAWS(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.AWS == nil || nodeClass.Spec.AWS.AMI == "" {
		return nil, fmt.Errorf("NodeClass spec.aws with ami is required for AWS provider")
	}
	var accessKeyID, secretAccessKey string
	if nodeClass.Spec.AWS.AccessKeyIDSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.AWS.AccessKeyIDSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get AWS access key ID secret: %w", err)
		}
		k := nodeClass.Spec.AWS.AccessKeyIDSecretRef.Key
		if k == "" {
			k = "AWS_ACCESS_KEY_ID"
		}
		accessKeyID = string(secret.Data[k])
	}
	if nodeClass.Spec.AWS.SecretAccessKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.AWS.SecretAccessKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get AWS secret access key secret: %w", err)
		}
		k := nodeClass.Spec.AWS.SecretAccessKeySecretRef.Key
		if k == "" {
			k = "AWS_SECRET_ACCESS_KEY"
		}
		secretAccessKey = string(secret.Data[k])
	}
	if accessKeyID == "" {
		accessKeyID = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if secretAccessKey == "" {
		secretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	return aws.NewProvisioner(ctx, region, accessKeyID, secretAccessKey,
		nodeClass.Spec.AWS.SubnetID,
		nodeClass.Spec.AWS.SecurityGroupIDs,
		nodeClass.Spec.AWS.InstanceProfileARN)
}

func newAzure(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, location string) (Provisioner, error) {
	if nodeClass.Spec.Azure == nil {
		return nil, fmt.Errorf("NodeClass spec.azure is required for Azure provider")
	}
	subID := nodeClass.Spec.Azure.SubscriptionID
	rg := nodeClass.Spec.Azure.ResourceGroup
	subnetID := nodeClass.Spec.Azure.SubnetID
	if subID == "" || rg == "" || location == "" {
		return nil, fmt.Errorf("NodeClass spec.azure subscriptionID, resourceGroup, and location (from recommendation) are required")
	}
	// subnetID may be empty; when empty, the provisioner auto-creates VNet and subnet
	var tenantID, clientID, clientSecret string
	if nodeClass.Spec.Azure.TenantIDSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Azure.TenantIDSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Azure tenant ID secret: %w", err)
		}
		k := nodeClass.Spec.Azure.TenantIDSecretRef.Key
		if k == "" {
			k = "AZURE_TENANT_ID"
		}
		tenantID = string(secret.Data[k])
	}
	if nodeClass.Spec.Azure.ClientIDSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Azure.ClientIDSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Azure client ID secret: %w", err)
		}
		k := nodeClass.Spec.Azure.ClientIDSecretRef.Key
		if k == "" {
			k = "AZURE_CLIENT_ID"
		}
		clientID = string(secret.Data[k])
	}
	if nodeClass.Spec.Azure.ClientSecretSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Azure.ClientSecretSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Azure client secret: %w", err)
		}
		k := nodeClass.Spec.Azure.ClientSecretSecretRef.Key
		if k == "" {
			k = "AZURE_CLIENT_SECRET"
		}
		clientSecret = string(secret.Data[k])
	}
	if tenantID == "" {
		tenantID = os.Getenv("AZURE_TENANT_ID")
	}
	if clientID == "" {
		clientID = os.Getenv("AZURE_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("AZURE_CLIENT_SECRET")
	}
	return azure.NewProvisioner(ctx, subID, rg, location, subnetID, tenantID, clientID, clientSecret)
}

func newAruba(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.Aruba == nil || nodeClass.Spec.Aruba.ProjectID == "" {
		return nil, fmt.Errorf("NodeClass spec.aruba with projectID is required for Aruba provider")
	}
	var appKey, appSecret string
	if nodeClass.Spec.Aruba.ApplicationKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Aruba.ApplicationKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Aruba application key secret: %w", err)
		}
		k := nodeClass.Spec.Aruba.ApplicationKeySecretRef.Key
		if k == "" {
			k = "ARUBA_API_KEY"
		}
		appKey = string(secret.Data[k])
		if appKey == "" {
			appKey = string(secret.Data["api_key"])
		}
	}
	if nodeClass.Spec.Aruba.ApplicationSecretSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Aruba.ApplicationSecretSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Aruba application secret: %w", err)
		}
		k := nodeClass.Spec.Aruba.ApplicationSecretSecretRef.Key
		if k == "" {
			k = "ARUBA_API_SECRET"
		}
		appSecret = string(secret.Data[k])
		if appSecret == "" {
			appSecret = string(secret.Data["api_secret"])
		}
	}
	if appKey == "" {
		appKey = os.Getenv("ARUBA_API_KEY")
	}
	if appSecret == "" {
		appSecret = os.Getenv("ARUBA_API_SECRET")
	}
	if appKey == "" || appSecret == "" {
		return nil, fmt.Errorf("Aruba credentials required: set spec.aruba secret refs or ARUBA_API_KEY, ARUBA_API_SECRET env vars")
	}
	return aruba.NewProvisioner(ctx, "https://api.arubacloud.com", appKey, appSecret, "", nodeClass.Spec.Aruba.ProjectID)
}

func newUpCloud(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, zone string) (Provisioner, error) {
	if nodeClass.Spec.UpCloud == nil {
		return nil, fmt.Errorf("NodeClass spec.upcloud is required for UpCloud provider")
	}
	var apiToken string
	if nodeClass.Spec.UpCloud.APITokenSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.UpCloud.APITokenSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get UpCloud API token secret: %w", err)
		}
		k := nodeClass.Spec.UpCloud.APITokenSecretRef.Key
		if k == "" {
			k = "UPCLOUD_API_TOKEN"
		}
		apiToken = string(secret.Data[k])
		if apiToken == "" {
			apiToken = string(secret.Data["UPCLOUD_TOKEN"])
		}
	}
	if apiToken == "" {
		apiToken = os.Getenv("UPCLOUD_API_TOKEN")
	}
	if apiToken == "" {
		apiToken = os.Getenv("UPCLOUD_TOKEN")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("no UpCloud API token: set spec.upcloud.apiTokenSecretRef or UPCLOUD_API_TOKEN env var")
	}
	return upcloud.NewProvisioner(ctx, apiToken, zone)
}

func newExoscale(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, zone string) (Provisioner, error) {
	if nodeClass.Spec.Exoscale == nil {
		return nil, fmt.Errorf("NodeClass spec.exoscale is required for Exoscale provider")
	}
	var apiKey, apiSecret string
	if nodeClass.Spec.Exoscale.APIKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Exoscale.APIKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Exoscale API key secret: %w", err)
		}
		k := nodeClass.Spec.Exoscale.APIKeySecretRef.Key
		if k == "" {
			k = "EXOSCALE_API_KEY"
		}
		apiKey = string(secret.Data[k])
	}
	if nodeClass.Spec.Exoscale.APISecretSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeClass.Spec.Exoscale.APISecretSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Exoscale API secret: %w", err)
		}
		k := nodeClass.Spec.Exoscale.APISecretSecretRef.Key
		if k == "" {
			k = "EXOSCALE_API_SECRET"
		}
		apiSecret = string(secret.Data[k])
	}
	if apiKey == "" {
		apiKey = os.Getenv("EXOSCALE_API_KEY")
	}
	if apiSecret == "" {
		apiSecret = os.Getenv("EXOSCALE_API_SECRET")
	}
	if apiKey == "" || apiSecret == "" {
		return nil, fmt.Errorf("Exoscale credentials required: set spec.exoscale.apiKeySecretRef and apiSecretSecretRef or EXOSCALE_API_KEY, EXOSCALE_API_SECRET env vars")
	}
	exoscaleZone := zone
	if nodeClass.Spec.Exoscale.Zone != "" {
		exoscaleZone = nodeClass.Spec.Exoscale.Zone
	}
	return exoscale.NewProvisioner(ctx, apiKey, apiSecret, exoscaleZone, nodeClass.Spec.Exoscale.DiskSizeGB)
}

func newSeeweb(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.Seeweb == nil {
		return nil, fmt.Errorf("NodeClass spec.seeweb is required for Seeweb provider")
	}
	cfg := nodeClass.Spec.Seeweb
	var apiToken string
	if cfg.APITokenSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APITokenSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Seeweb API token secret: %w", err)
		}
		k := cfg.APITokenSecretRef.Key
		if k == "" {
			k = "SEEWEB_API_TOKEN"
		}
		apiToken = string(secret.Data[k])
	}
	if apiToken == "" {
		apiToken = os.Getenv("SEEWEB_API_TOKEN")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("Seeweb API token required: set spec.seeweb.apiTokenSecretRef or SEEWEB_API_TOKEN env var")
	}
	datacenter := cfg.Datacenter
	if datacenter == "" {
		datacenter = region
	}
	return seeweb.NewProvisioner(ctx, apiToken, datacenter)
}

func newContabo(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.Contabo == nil {
		return nil, fmt.Errorf("NodeClass spec.contabo is required for Contabo provider")
	}
	cfg := nodeClass.Spec.Contabo
	var clientID, clientSecret, apiUser, apiPassword string
	if cfg.ClientIDSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.ClientIDSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Contabo client ID secret: %w", err)
		}
		clientID = string(secret.Data[cfg.ClientIDSecretRef.Key])
		if clientID == "" {
			clientID = string(secret.Data["CONTABO_CLIENT_ID"])
		}
	}
	if cfg.ClientSecretSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.ClientSecretSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Contabo client secret: %w", err)
		}
		clientSecret = string(secret.Data[cfg.ClientSecretSecretRef.Key])
		if clientSecret == "" {
			clientSecret = string(secret.Data["CONTABO_CLIENT_SECRET"])
		}
	}
	if cfg.APIUserSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APIUserSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Contabo API user secret: %w", err)
		}
		apiUser = string(secret.Data[cfg.APIUserSecretRef.Key])
		if apiUser == "" {
			apiUser = string(secret.Data["CONTABO_API_USER"])
		}
	}
	if cfg.APIPasswordSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APIPasswordSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Contabo API password secret: %w", err)
		}
		apiPassword = string(secret.Data[cfg.APIPasswordSecretRef.Key])
		if apiPassword == "" {
			apiPassword = string(secret.Data["CONTABO_API_PASSWORD"])
		}
	}
	if clientID == "" {
		clientID = os.Getenv("CONTABO_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("CONTABO_CLIENT_SECRET")
	}
	if apiUser == "" {
		apiUser = os.Getenv("CONTABO_API_USER")
	}
	if apiPassword == "" {
		apiPassword = os.Getenv("CONTABO_API_PASSWORD")
	}
	if clientID == "" || clientSecret == "" || apiUser == "" || apiPassword == "" {
		return nil, fmt.Errorf("Contabo credentials required: set spec.contabo secret refs or CONTABO_* env vars")
	}
	return contabo.NewProvisioner(ctx, clientID, clientSecret, apiUser, apiPassword)
}

func newGcore(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.Gcore == nil || nodeClass.Spec.Gcore.ProjectID <= 0 {
		return nil, fmt.Errorf("NodeClass spec.gcore with projectID is required for Gcore provider")
	}
	cfg := nodeClass.Spec.Gcore
	var apiKey string
	if cfg.APIKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APIKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Gcore API key secret: %w", err)
		}
		k := cfg.APIKeySecretRef.Key
		if k == "" {
			k = "GCORE_API_KEY"
		}
		apiKey = string(secret.Data[k])
	}
	if apiKey == "" {
		apiKey = os.Getenv("GCORE_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("Gcore API key required: set spec.gcore.apiKeySecretRef or GCORE_API_KEY env var")
	}
	return gcore.NewProvisioner(ctx, apiKey, int(cfg.ProjectID))
}

func newCherryServers(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.CherryServers == nil || nodeClass.Spec.CherryServers.ProjectID <= 0 {
		return nil, fmt.Errorf("NodeClass spec.cherryServers with projectID is required for Cherry Servers provider")
	}
	cfg := nodeClass.Spec.CherryServers
	var apiToken string
	if cfg.APITokenSecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APITokenSecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Cherry Servers API token secret: %w", err)
		}
		k := cfg.APITokenSecretRef.Key
		if k == "" {
			k = "CHERRY_SERVERS_API_TOKEN"
		}
		apiToken = string(secret.Data[k])
	}
	if apiToken == "" {
		apiToken = os.Getenv("CHERRY_SERVERS_API_TOKEN")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("Cherry Servers API token required: set spec.cherryServers.apiTokenSecretRef or CHERRY_SERVERS_API_TOKEN env var")
	}
	return cherry_servers.NewProvisioner(ctx, apiToken, int(cfg.ProjectID))
}

func newClouding(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.Clouding == nil {
		return nil, fmt.Errorf("NodeClass spec.clouding is required for Clouding.io provider")
	}
	cfg := nodeClass.Spec.Clouding
	var apiKey string
	if cfg.APIKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APIKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get Clouding.io API key secret: %w", err)
		}
		k := cfg.APIKeySecretRef.Key
		if k == "" {
			k = "CLOUDING_API_KEY"
		}
		apiKey = string(secret.Data[k])
	}
	if apiKey == "" {
		apiKey = os.Getenv("CLOUDING_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("Clouding.io API key required: set spec.clouding.apiKeySecretRef or CLOUDING_API_KEY env var")
	}
	return clouding.NewProvisioner(ctx, apiKey, cfg.FirewallID)
}

func newIBMCloud(ctx context.Context, nodeClass *cloudburstv1alpha1.NodeClass, namespace string, c Getter, region string) (Provisioner, error) {
	if nodeClass.Spec.IBMCloud == nil || nodeClass.Spec.IBMCloud.ResourceGroup == "" {
		return nil, fmt.Errorf("NodeClass spec.ibmCloud with resourceGroup is required for IBM Cloud provider")
	}
	cfg := nodeClass.Spec.IBMCloud
	var apiKey string
	if cfg.APIKeySecretRef != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cfg.APIKeySecretRef.Name}, secret); err != nil {
			return nil, fmt.Errorf("failed to get IBM Cloud API key secret: %w", err)
		}
		k := cfg.APIKeySecretRef.Key
		if k == "" {
			k = "IBMCLOUD_API_KEY"
		}
		apiKey = string(secret.Data[k])
	}
	if apiKey == "" {
		apiKey = os.Getenv("IBMCLOUD_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("IBM Cloud API key required: set spec.ibmCloud.apiKeySecretRef or IBMCLOUD_API_KEY env var")
	}
	loc := cfg.Region
	if loc == "" {
		loc = region
	}
	return ibmcloud.NewProvisioner(ctx, apiKey, loc, cfg.ResourceGroup, cfg.SubnetID, cfg.SecurityGroups)
}
