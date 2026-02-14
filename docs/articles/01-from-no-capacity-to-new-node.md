# When Your Cluster Runs Out of Room: Cloudburst Autoscaler Adds the Cheapest Node

**The problem and the product: cost-aware, multi-cloud burst for Kubernetes.**

---

A pod is stuck in "pending." No node has enough CPU or memory. Normally a human adds a node — pick a provider, create a VM, join it, remember to delete it later. Cost? Whatever that one provider charges. **Cloudburst Autoscaler** automates that and bakes cost into the decision: it asks a recommendation API for the cheapest instance that fits, provisions that VM, turns it into a node, and tears it down when the workload is gone. No manual resizing; no single-cloud default.

---

## The capacity problem

Kubernetes leaves the pod **Pending** with reason **Unschedulable**. Someone has to add capacity. In practice that's manual, repetitive, and rarely optimized for price. You resize a node group in one cloud, or you spin up a VM by hand and run `kubeadm join`. Multi-cloud and cost-aware burst require a system that can compare options across providers and then provision the chosen one. Doing that by hand every time doesn't scale.

---

## What Cloudburst Autoscaler does

The Autoscaler **watches** for unschedulable pods and **aggregates** demand: how much CPU and RAM do those pods need in total? It sends that, plus your policy (allowed providers, max price, region), to a **recommendation API**. The API returns a ranked answer: "use this instance type, in this region, on this provider." The Autoscaler **provisions** that VM on the right cloud, injects a **bootstrap script** so the machine installs Tailscale (to reach the control plane), containerd, kubelet, and kubeadm, then runs `kubeadm join`. The VM becomes a node. The scheduler places the pod. When the node has been empty long enough, the controller **cordons**, **drains**, deletes the node object, and deletes the VM. Billing stops. All of that is automatic.

---

## Why this shape

The system splits two concerns. **"What's cheapest?"** lives in a recommendation API (e.g. **CloudBroker**): one service that knows instance types and hourly rates across seven clouds and answers "what do you recommend for these constraints?" **"Create and delete the VM"** lives in the Autoscaler: it doesn't own pricing logic; it just calls the API and provisions the top result. That separation keeps the Autoscaler flexible. You can swap or tune the recommendation service without changing how the controller provisions. The Autoscaler is the provisioner; the API is the price brain.

---

The next piece explains how it works end to end: the flow from unschedulable pod to new node, and how the VM becomes a node (Tailscale, bootstrap, join).
