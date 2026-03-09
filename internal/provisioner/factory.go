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
	"github.com/diegobraga/cloudburst-autoscaler/internal/digitalocean"
	"github.com/diegobraga/cloudburst-autoscaler/internal/gcp"
	"github.com/diegobraga/cloudburst-autoscaler/internal/hetzner"
	"github.com/diegobraga/cloudburst-autoscaler/internal/ovh"
	"github.com/diegobraga/cloudburst-autoscaler/internal/scaleway"
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
	if subID == "" || rg == "" || location == "" || subnetID == "" {
		return nil, fmt.Errorf("NodeClass spec.azure subscriptionID, resourceGroup, location (from recommendation), and subnetID are required")
	}
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
