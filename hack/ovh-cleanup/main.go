// ovh-cleanup is a standalone utility to list and delete orphaned cb-* OVH Cloud instances.
// Used by E2E tests for pre-cleanup and post-verification.
//
// Environment: OVH_APPLICATION_KEY, OVH_APPLICATION_SECRET, OVH_CONSUMER_KEY, OVH_PROJECT_ID
//
// Usage:
//
//	go run ./hack/ovh-cleanup/main.go
//	    List and delete all cb-* instances in the project.
//
//	go run ./hack/ovh-cleanup/main.go --verify=ovh/projectId/instanceId
//	    Verify that the given instance no longer exists (exit 0 if gone, 1 if still exists).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/diegobraga/cloudburst-autoscaler/internal/ovh"
)

func main() {
	verify := flag.String("verify", "", "Instance ID to verify is deleted (ovh/projectId/instanceId)")
	listOnly := flag.Bool("list", false, "List all instances (don't delete)")
	flag.Parse()

	appKey := os.Getenv("OVH_APPLICATION_KEY")
	appSecret := os.Getenv("OVH_APPLICATION_SECRET")
	consumerKey := os.Getenv("OVH_CONSUMER_KEY")
	projectID := os.Getenv("OVH_PROJECT_ID")
	if appKey == "" || appSecret == "" || consumerKey == "" || projectID == "" {
		fmt.Fprintln(os.Stderr, "OVH_APPLICATION_KEY, OVH_APPLICATION_SECRET, OVH_CONSUMER_KEY, OVH_PROJECT_ID required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prov, err := ovh.NewProvisioner(ctx, "ovh-eu", appKey, appSecret, consumerKey, projectID, "GRA7") // region required for constructor, not used for List
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create OVH client: %v\n", err)
		os.Exit(1)
	}
	defer prov.Close()

	if *verify != "" {
		status, err := prov.GetInstanceStatus(ctx, *verify)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "404") || strings.Contains(err.Error(), "not found") {
				fmt.Printf("Instance %s confirmed deleted\n", *verify)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "Error checking instance: %v\n", err)
			os.Exit(1)
		}
		// OVH/OpenStack can return DELETING or DELETED while instance is being torn down
		switch strings.ToUpper(status) {
		case "DELETING", "DELETED":
			fmt.Printf("Instance %s is %s (effectively deleted)\n", *verify, status)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Instance %s still exists (status=%s)\n", *verify, status)
		os.Exit(1)
	}

	instances, err := prov.ListInstances(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list instances: %v\n", err)
		os.Exit(1)
	}

	if *listOnly {
		fmt.Printf("Found %d instance(s) in OVH project %s:\n", len(instances), projectID)
		for _, inst := range instances {
			fmt.Printf("  - %s (id=%s, status=%s, region=%s) cb-prefix=%v\n",
				inst.Name, inst.ID, inst.Status, inst.Region, strings.HasPrefix(inst.Name, "cb-"))
		}
		return
	}

	var deleted int
	for _, inst := range instances {
		if strings.HasPrefix(inst.Name, "cb-") {
			instanceID := fmt.Sprintf("ovh/%s/%s", projectID, inst.ID)
			fmt.Printf("Deleting %s (%s)...\n", inst.Name, instanceID)
			if err := prov.DeleteInstance(ctx, instanceID); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to delete %s: %v\n", instanceID, err)
				os.Exit(1)
			}
			deleted++
		}
	}

	if deleted > 0 {
		fmt.Printf("Deleted %d cb-* instance(s)\n", deleted)
	} else {
		fmt.Println("No cb-* instances found")
	}
}
