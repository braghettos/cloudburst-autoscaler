// aruba-cleanup is a standalone utility to list and delete orphaned cb-* Aruba Cloud instances.
// Used by E2E tests for pre-cleanup and post-verification.
//
// Environment: ARUBA_API_KEY, ARUBA_API_SECRET, ARUBA_PROJECT_ID
//
// Usage:
//
//	go run ./hack/aruba-cleanup/main.go
//	    List and delete all cb-* instances in the project.
//
//	go run ./hack/aruba-cleanup/main.go --verify=aruba/projectId/instanceId
//	    Verify that the given instance no longer exists (exit 0 if gone, 1 if still exists).
//
//	go run ./hack/aruba-cleanup/main.go --verify-no-orphans
//	    Verify no cb-* instances or cb-*-boot volumes remain (exit 0 if clean, 1 if any found).
//
//	go run ./hack/aruba-cleanup/main.go --cleanup-orphans
//	    Delete orphaned cb-*-boot volumes (no instance deletion).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/diegobraga/cloudburst-autoscaler/internal/aruba"
)

func main() {
	verify := flag.String("verify", "", "Instance ID to verify is deleted (aruba/projectId/instanceId)")
	verifyNoOrphans := flag.Bool("verify-no-orphans", false, "Verify no cb-* instances or cb-*-boot volumes remain")
	cleanupOrphans := flag.Bool("cleanup-orphans", false, "Delete orphaned cb-*-boot volumes")
	listOnly := flag.Bool("list", false, "List all instances (don't delete)")
	flag.Parse()

	appKey := os.Getenv("ARUBA_API_KEY")
	appSecret := os.Getenv("ARUBA_API_SECRET")
	projectID := os.Getenv("ARUBA_PROJECT_ID")
	if appKey == "" || appSecret == "" || projectID == "" {
		fmt.Fprintln(os.Stderr, "ARUBA_API_KEY, ARUBA_API_SECRET, ARUBA_PROJECT_ID required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prov, err := aruba.NewProvisioner(ctx, "", appKey, appSecret, "", projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Aruba client: %v\n", err)
		os.Exit(1)
	}
	defer prov.Close()

	if *cleanupOrphans {
		vols, _, _, err := prov.CleanupOrphanedResources(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cleanup failed: %v\n", err)
			os.Exit(1)
		}
		if vols > 0 {
			fmt.Printf("Cleaned up: %d volume(s)\n", vols)
		} else {
			fmt.Println("No orphaned resources to clean")
		}
		return
	}

	if *verifyNoOrphans {
		instances, err := prov.ListInstances(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing instances: %v\n", err)
			os.Exit(1)
		}
		volumes, err := prov.ListCloudBurstVolumes(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing volumes: %v\n", err)
			os.Exit(1)
		}
		var cbInstances []string
		for _, inst := range instances {
			if strings.HasPrefix(inst.Name, "cb-") {
				cbInstances = append(cbInstances, inst.Name)
			}
		}
		if len(cbInstances) > 0 {
			fmt.Fprintf(os.Stderr, "Found %d cb-* instance(s) still present: %v\n", len(cbInstances), cbInstances)
			os.Exit(1)
		}
		if len(volumes) > 0 {
			fmt.Fprintf(os.Stderr, "Found %d cb-*-boot volume(s) still present: %v\n", len(volumes), volumes)
			os.Exit(1)
		}
		fmt.Println("No orphaned cb-* instances or cb-*-boot volumes found")
		return
	}

	if *verify != "" {
		instances, err := prov.ListInstances(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing instances: %v\n", err)
			os.Exit(1)
		}
		// Extract instance ID from verify (aruba/projectId/instanceId)
		parts := strings.SplitN(*verify, "/", 3)
		verifyID := ""
		if len(parts) == 3 {
			verifyID = parts[2]
		} else {
			verifyID = *verify
		}
		for _, inst := range instances {
			if inst.ID == verifyID {
				fmt.Fprintf(os.Stderr, "Instance %s still exists\n", *verify)
				os.Exit(1)
			}
		}
		fmt.Printf("Instance %s confirmed deleted\n", *verify)
		return
	}

	instances, err := prov.ListInstances(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list instances: %v\n", err)
		os.Exit(1)
	}

	if *listOnly {
		fmt.Printf("Found %d instance(s) in Aruba project %s:\n", len(instances), projectID)
		for _, inst := range instances {
			fmt.Printf("  - %s (id=%s, status=%s, region=%s) cb-prefix=%v\n",
				inst.Name, inst.ID, inst.Status, inst.Region, strings.HasPrefix(inst.Name, "cb-"))
		}
		return
	}

	var deleted int
	for _, inst := range instances {
		if strings.HasPrefix(inst.Name, "cb-") {
			instanceID := aruba.InstanceIDPrefix + projectID + "/" + inst.ID
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

	// Clean up orphaned volumes
	vols, _, _, err := prov.CleanupOrphanedResources(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cleanup of orphaned resources failed: %v\n", err)
	} else if vols > 0 {
		fmt.Printf("Cleaned up orphaned resources: %d volume(s)\n", vols)
	}
}
