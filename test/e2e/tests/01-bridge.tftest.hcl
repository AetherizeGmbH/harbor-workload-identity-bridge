# End-to-end test of the Harbor Workload Identity Bridge.
#
# Stages (each `run` block is a separate plan+apply, sharing state):
#   1. cluster              — kind cluster with cilium + cert-manager + containerd skip_verify
#   2. harbor               — Harbor via official helm chart, exposed via NodePort 30843
#   3. seed_image           — push alpine:3.20 → harbor.your-project/alpine:test3
#   4. bridge_install       — our chart (creates ClusterIssuer, CRD, Deployment, DaemonSet)
#   5. harbor_access        — apply a HarborAccess CR for the test SA
#   6. pull_pod             — Job pulling the image; success = end-to-end works
#
# Rather than fighting Traefik + CoreDNS rewrites + cert SANs, we
# expose Harbor on a NodePort that's reachable from kind nodes' host
# netns via Cilium. Image refs use 127.0.0.1:30843 so containerd's
# default DNS resolves locally and the NodePort listener answers.
# Self-signed cert verification is skipped via containerd's hosts.toml
# (configured in the kind-cluster module).
#
# The last stage is the load-bearing assertion. If the kubelet
# silent-abort bug from docs/PHASES.md still bites, the Job will fail
# with ImagePullBackOff and the test fails.

run "cluster" {
  module {
    source = "./modules/kind-cluster"
  }
  variables {
    name                        = "bridge-e2e"
    worker_count                = 2
    http_port                   = 8080
    https_port                  = 8443
    api_server_port             = 6443
    enable_cert_manager         = true
    registry_insecure_hostnames = ["127.0.0.1:30843"]
    # Copy locally-built images into the cluster so the bridge install
    # uses them without a registry. CI builds these in the workflow
    # step that runs before `tofu test`.
    images_to_load = [
      "harbor-bridge:e2e",
      "harbor-bridge-plugin:e2e",
    ]
  }
}

run "harbor" {
  module {
    source = "./modules/harbor"
  }
  variables {
    kubeconfig        = run.cluster.kubeconfig
    external_hostname = "127.0.0.1"
    https_node_port   = 30843
    http_node_port    = 30880
  }
}

# Seed the test image into Harbor. The Job runs crane inside the
# cluster, talking to harbor-core via service DNS over HTTP:80.
run "seed_image" {
  command = apply
  module {
    source = "./modules/k8s-yaml"
  }
  variables {
    kubeconfig = run.cluster.kubeconfig
    manifests = [
      <<-YAML
        apiVersion: v1
        kind: Namespace
        metadata: { name: e2e-seed }
      YAML
      ,
      <<-YAML
        apiVersion: v1
        kind: Secret
        metadata:
          name: harbor-admin
          namespace: e2e-seed
        stringData:
          username: admin
          password: ${run.harbor.admin_password}
        type: Opaque
      YAML
      ,
      <<-YAML
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: project-bootstrap
          namespace: e2e-seed
        data:
          run.sh: |
            #!/bin/sh
            set -e
            # Create the project via in-cluster HTTP path. Harbor core
            # service serves the REST API on plain HTTP:80.
            curl -sv -u "admin:$(cat /admin/password)" -X POST \
              -H "Content-Type: application/json" \
              http://harbor-core.harbor.svc.cluster.local/api/v2.0/projects \
              -d '{"project_name":"your-project","metadata":{"public":"false"}}' || true
            # Push alpine:3.20 to your-project/alpine:test3. --insecure
            # makes crane use HTTP for the harbor-core service.
            crane auth login harbor-core.harbor.svc.cluster.local \
              -u admin -p "$(cat /admin/password)" --insecure
            crane copy alpine:3.20 \
              harbor-core.harbor.svc.cluster.local/your-project/alpine:test3 \
              --insecure
      YAML
      ,
      <<-YAML
        apiVersion: batch/v1
        kind: Job
        metadata:
          name: seed-image
          namespace: e2e-seed
        spec:
          backoffLimit: 2
          template:
            spec:
              restartPolicy: Never
              containers:
                - name: seed
                  image: gcr.io/go-containerregistry/crane:v0.20.2
                  command: [sh, /scripts/run.sh]
                  volumeMounts:
                    - { name: scripts, mountPath: /scripts }
                    - { name: admin,   mountPath: /admin, readOnly: true }
              volumes:
                - name: scripts
                  configMap:
                    name: project-bootstrap
                    defaultMode: 0755
                - name: admin
                  secret:
                    secretName: harbor-admin
      YAML
    ]
  }
}

run "bridge_install" {
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    kubeconfig            = run.cluster.kubeconfig
    cluster_name          = "dev"
    harbor_url            = run.harbor.internal_api_url
    harbor_admin_password = run.harbor.admin_password
    audience              = "harbor-bridge"
    match_images          = ["127.0.0.1:30843/*"]
  }
}

run "harbor_access" {
  command = apply
  module {
    source = "./modules/k8s-yaml"
  }
  variables {
    kubeconfig = run.cluster.kubeconfig
    manifests = [
      <<-YAML
        apiVersion: v1
        kind: Namespace
        metadata: { name: test-pull }
      YAML
      ,
      <<-YAML
        apiVersion: v1
        kind: ServiceAccount
        metadata:
          name: image-puller
          namespace: test-pull
      YAML
      ,
      <<-YAML
        apiVersion: harbor.aetherize.io/v1alpha1
        kind: HarborAccess
        metadata:
          name: test-access
          namespace: ${run.bridge_install.namespace}
        spec:
          serviceAccountRef:
            namespace: test-pull
            name: image-puller
          trustPolicy:
            issuer: https://kubernetes.default.svc.cluster.local
            audience: harbor-bridge
          permissions:
            - project: your-project
              action: pull
          tokenTTL: 1h
      YAML
    ]
  }
}

# Load-bearing assertion. Image ref uses 127.0.0.1:30843 — reachable
# from every kind node's host netns via Cilium NodePort listener.
# Success = kubelet fork+execs the plugin AND the plugin returns valid
# Harbor robot credentials AND containerd accepts them.
run "pull_pod" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "bridge-pull-test"
    namespace            = "test-pull"
    service_account_name = "image-puller"
    image                = "127.0.0.1:30843/your-project/alpine:test3"
    command              = ["sh", "-c"]
    args                 = ["echo bridge-pull-test pulled successfully; exit 0"]
    timeout_seconds      = 300
    fail_message         = "kubelet silent-abort bug: see docs/PHASES.md §kubelet-exec-blocker"
  }
}
