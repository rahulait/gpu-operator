#!/usr/bin/env bash

set -o nounset
set -x

K=kubectl
if ! $K version > /dev/null; then
    K=oc

    if ! $K version > /dev/null; then
        echo "FATAL: neither 'kubectl' nor 'oc' appear to be working properly. Exiting ..."
        exit 1
    fi
fi

if [[ "$0" == "/usr/bin/gather" ]]; then
    echo "Running as must-gather plugin image"
    export ARTIFACT_DIR=/must-gather
else
    if [ -z "${ARTIFACT_DIR:-}" ]; then
        export ARTIFACT_DIR="/tmp/nvidia-gpu-operator_$(date +%Y%m%d_%H%M)"
    fi
    echo "Using ARTIFACT_DIR=${ARTIFACT_DIR}"
fi

mkdir -p "${ARTIFACT_DIR}"

echo

exec 1> >(tee "${ARTIFACT_DIR}/must-gather.log")
exec 2> "${ARTIFACT_DIR}/must-gather.stderr.log"

if [[ "$0" == "/usr/bin/gather" ]]; then
    echo "NVIDIA GPU Operator" > "${ARTIFACT_DIR}/version"
    echo "${VERSION:-N/A}" >> "${ARTIFACT_DIR}/version"
fi

ocp_cluster=$(timeout 10s $K get clusterversion/version --ignore-not-found -oname || true)

if [[ "$ocp_cluster" ]]; then
    echo "Running in OpenShift."
    echo "Get the cluster version"
    timeout 30s $K get clusterversion/version -oyaml > "${ARTIFACT_DIR}/openshift_version.yaml" || echo "Timeout getting OpenShift version"
fi

echo
echo "#"
echo "# KubeVirt HyperConverged Resources"
echo "#"
echo

HYPERCONVERGED_RESOURCE=$(timeout 10s $K get hyperconvergeds.hco.kubevirt.io -A -oname --ignore-not-found || true)

if [[ "$HYPERCONVERGED_RESOURCE" ]]; then
    echo "Get HyperConverged YAML"
    timeout 30s $K get hyperconvergeds.hco.kubevirt.io -A -oyaml > $ARTIFACT_DIR/hyperconverged.yaml || echo "Timeout getting HyperConverged YAML"
else
    echo "HyperConverged resource(s) not found in the cluster."
fi

echo "Get the operator namespaces"
OPERATOR_POD_NAME=$(timeout 30s $K get pods -lapp=gpu-operator -oname -A || true)

if [ -z "$OPERATOR_POD_NAME" ]; then
    echo "FATAL: could not find the GPU Operator Pod ..."
    exit 1
fi

OPERATOR_NAMESPACE=$(timeout 10s $K get pods -lapp=gpu-operator -A -ojsonpath='{.items[].metadata.namespace}' --ignore-not-found || true)

echo "Using '$OPERATOR_NAMESPACE' as operator namespace"
echo ""

echo
echo "#"
echo "# KubeVirt Resources"
echo "#"
echo

KUBEVIRT_RESOURCE=$(timeout 10s $K get kubevirts.kubevirt.io -A -oname --ignore-not-found || true)

if [[ "$KUBEVIRT_RESOURCE" ]]; then
    echo "Get KubeVirt YAML"
    timeout 30s $K get kubevirts.kubevirt.io -A -oyaml > $ARTIFACT_DIR/kubevirt.yaml || echo "Timeout getting KubeVirt YAML"
else
    echo "KubeVirt resource(s) not found in the cluster."
fi

echo "#"
echo "# ClusterPolicy"
echo "#"
echo

CLUSTER_POLICY_NAME=$(timeout 10s $K get clusterpolicies.nvidia.com -oname || true)

if [[ "${CLUSTER_POLICY_NAME}" ]]; then
    echo "Get ${CLUSTER_POLICY_NAME}"
    timeout 30s $K get -oyaml "${CLUSTER_POLICY_NAME}" > "${ARTIFACT_DIR}/cluster_policy.yaml" || echo "Timeout getting ClusterPolicy"
else
    echo "Mark the ClusterPolicy as missing"
    touch "${ARTIFACT_DIR}/cluster_policy.missing"
fi

echo
echo "#"
echo "# NVIDIADriver"
echo "#"
echo

NVIDIA_DRIVERS=$(timeout 10s $K get nvidiadrivers.nvidia.com -A -oname || true)

if [[ "${NVIDIA_DRIVERS}" ]]; then
    echo "Get NVIDIADriver resources"
    timeout 30s $K get nvidiadrivers.nvidia.com -A -oyaml > "${ARTIFACT_DIR}/nvidiadrivers.yaml" || echo "Timeout getting NVIDIADriver resources"
else
    echo "NVIDIADriver resource(s) not found in the cluster."
fi

echo
echo "#"
echo "# Nodes and machines"
echo "#"
echo

if [ "$ocp_cluster" ]; then
    echo "Get all the machines"
    timeout 30s $K get machines -A > "${ARTIFACT_DIR}/all_machines.list" || echo "Timeout getting machines"
fi

echo "Get the labels of the nodes with NVIDIA PCI cards"

GPU_PCI_LABELS=(feature.node.kubernetes.io/pci-10de.present feature.node.kubernetes.io/pci-0302_10de.present feature.node.kubernetes.io/pci-0300_10de.present)

gpu_pci_nodes=""
for label in "${GPU_PCI_LABELS[@]}"; do
    gpu_pci_nodes="$gpu_pci_nodes $(timeout 10s $K get nodes -l$label -oname || true)"
done

if [ -z "$gpu_pci_nodes" ]; then
    echo "FATAL: could not find nodes with NVIDIA PCI labels"
    exit 0
fi

for node in $(echo "$gpu_pci_nodes"); do
    echo "${node}" | cut -d/ -f2 >> "${ARTIFACT_DIR}/gpu_nodes.labels"
    timeout 10s $K get "${node}" '-ojsonpath={.metadata.labels}' \
        | sed 's|,|,- |g' \
        | tr ',' '\n' \
        | sed 's/{"/- /' \
        | tr : = \
        | sed 's/"//g' \
        | sed 's/}/\n/' \
              >> "${ARTIFACT_DIR}/gpu_nodes.labels" || echo "Timeout getting labels for ${node}" >> "${ARTIFACT_DIR}/gpu_nodes.labels"
    echo "" >> "${ARTIFACT_DIR}/gpu_nodes.labels"
done

echo "Get the GPU nodes (status)"
timeout 30s $K get nodes -l nvidia.com/gpu.present=true -o wide > "${ARTIFACT_DIR}/gpu_nodes.status" || echo "Timeout getting GPU nodes status"

echo "Get the GPU nodes (description)"
timeout 60s $K describe nodes -l nvidia.com/gpu.present=true > "${ARTIFACT_DIR}/gpu_nodes.descr" || echo "Timeout describing GPU nodes"

echo ""
echo "#"
echo "# Operator Pod"
echo "#"
echo

echo "Get the GPU Operator Pod (status)"
timeout 30s $K get "${OPERATOR_POD_NAME}" \
    -owide \
    -n "${OPERATOR_NAMESPACE}" \
    > "${ARTIFACT_DIR}/gpu_operator_pod.status" || echo "Timeout getting operator pod status"

echo "Get the GPU Operator Pod (yaml)"
timeout 30s $K get "${OPERATOR_POD_NAME}" \
    -oyaml \
    -n "${OPERATOR_NAMESPACE}" \
    > "${ARTIFACT_DIR}/gpu_operator_pod.yaml" || echo "Timeout getting operator pod YAML"

echo "Get the GPU Operator Pod logs"
timeout 60s $K logs "${OPERATOR_POD_NAME}" \
    -n "${OPERATOR_NAMESPACE}" \
    --timestamps \
    > "${ARTIFACT_DIR}/gpu_operator_pod.log" || echo "Timeout or error collecting operator logs"

timeout 60s $K logs "${OPERATOR_POD_NAME}" \
    -n "${OPERATOR_NAMESPACE}" \
    --timestamps \
    --previous \
    > "${ARTIFACT_DIR}/gpu_operator_pod.previous.log" 2>/dev/null || echo "No previous logs available for operator pod"

echo ""
echo "#"
echo "# Operand Pods"
echo "#"
echo ""

echo "Get the Pods in ${OPERATOR_NAMESPACE} (status)"
timeout 30s $K get pods -owide \
    -n "${OPERATOR_NAMESPACE}" \
    > "${ARTIFACT_DIR}/gpu_operand_pods.status" || echo "Timeout getting operand pods status"

echo "Get the Pods in ${OPERATOR_NAMESPACE} (yaml)"
timeout 30s $K get pods -oyaml \
    -n "${OPERATOR_NAMESPACE}" \
    > "${ARTIFACT_DIR}/gpu_operand_pods.yaml" || echo "Timeout getting operand pods YAML"

echo "Get the GPU Operator Pods Images"
timeout 30s $K get pods -n "${OPERATOR_NAMESPACE}" \
    -o=jsonpath='{range .items[*]}{"\n"}{.metadata.name}{":\t"}{range .spec.containers[*]}{.image}{" "}{end}{end}' \
    > "${ARTIFACT_DIR}/gpu_operand_pod_images.txt" || echo "Timeout getting pod images"

echo "Get the description and logs of the GPU Operator Pods"

for pod in $(timeout 30s $K get pods -n "${OPERATOR_NAMESPACE}" -oname || true); 
do
    if ! timeout 10s $K get "${pod}" -n "${OPERATOR_NAMESPACE}" -ojsonpath='{.metadata.labels}' | grep -E --quiet '(nvidia|gpu)'; then
        echo "Skipping $pod, not a NVIDA/GPU Pod ..."
        continue
    fi
    pod_name=$(echo "$pod" | cut -d/ -f2)

    if [ "${pod}" == "${OPERATOR_POD_NAME}" ]; then
        echo "Skipping operator pod $pod_name ..."
        continue
    fi

    timeout 60s $K logs "${pod}" \
        -n "${OPERATOR_NAMESPACE}" \
        --all-containers --prefix \
        --timestamps \
        > "${ARTIFACT_DIR}/gpu_operand_pod_$pod_name.log" || echo "Timeout or error collecting logs from $pod_name"

    timeout 60s $K logs "${pod}" \
        -n "${OPERATOR_NAMESPACE}" \
        --all-containers --prefix \
        --timestamps \
        --previous \
        > "${ARTIFACT_DIR}/gpu_operand_pod_$pod_name.previous.log" 2>/dev/null || true

    timeout 30s $K describe "${pod}" \
        -n "${OPERATOR_NAMESPACE}" \
        > "${ARTIFACT_DIR}/gpu_operand_pod_$pod_name.descr" || echo "Timeout describing $pod_name"
done

echo ""
echo "#"
echo "# Operand DaemonSets"
echo "#"
echo ""

echo "Get the DaemonSets in $OPERATOR_NAMESPACE (status)"

timeout 30s $K get ds \
    -n "${OPERATOR_NAMESPACE}" \
    > "${ARTIFACT_DIR}/gpu_operand_ds.status" || echo "Timeout getting DaemonSets status"

echo "Get the DaemonSets in $OPERATOR_NAMESPACE (yaml)"

timeout 30s $K get ds -oyaml \
    -n "${OPERATOR_NAMESPACE}" \
    > "${ARTIFACT_DIR}/gpu_operand_ds.yaml" || echo "Timeout getting DaemonSets YAML"

echo "Get the description of the GPU Operator DaemonSets"

for ds in $(timeout 30s $K get ds -n "${OPERATOR_NAMESPACE}" -oname || true);
do
    if ! timeout 10s $K get "${ds}" -n "${OPERATOR_NAMESPACE}" -ojsonpath='{.metadata.labels}' | grep -E --quiet '(nvidia|gpu)'; then
        echo "Skipping ${ds}, not a NVIDA/GPU DaemonSet ..."
        continue
    fi
    timeout 30s $K describe "${ds}" \
        -n "${OPERATOR_NAMESPACE}" \
        > "${ARTIFACT_DIR}/gpu_operand_ds_$(echo "$ds" | cut -d/ -f2).descr" || echo "Timeout describing DaemonSet ${ds}"
done

echo ""
echo "#"
echo "# nvidia-bug-report.sh"
echo "#"
echo ""

# Find driver pods using multiple label selectors to support different deployment methods
driver_pods=""
driver_pods="$driver_pods $(timeout 30s $K get pods -lopenshift.driver-toolkit -oname -n "${OPERATOR_NAMESPACE}" 2>/dev/null || true)"
driver_pods="$driver_pods $(timeout 30s $K get pods -lapp.kubernetes.io/component=nvidia-driver -oname -n "${OPERATOR_NAMESPACE}" 2>/dev/null || true)"
driver_pods="$driver_pods $(timeout 30s $K get pods -lapp=nvidia-vgpu-manager-daemonset -oname -n "${OPERATOR_NAMESPACE}" 2>/dev/null || true)"

# Deduplicate and filter out empty entries
driver_pods=$(echo "$driver_pods" | tr ' ' '\n' | grep -v '^$' | sort -u)

for pod in $driver_pods;
do
    pod_nodename=$(timeout 10s $K get "${pod}" -ojsonpath={.spec.nodeName} -n "${OPERATOR_NAMESPACE}" || echo "unknown")
    echo "Saving nvidia-bug-report from ${pod_nodename} ..."

    timeout 300s $K exec -n "${OPERATOR_NAMESPACE}" "${pod}" -- bash -c 'cd /tmp && nvidia-bug-report.sh' >&2 || \
        (echo "Failed to collect nvidia-bug-report from ${pod_nodename}" && continue)

    timeout 60s $K cp "${OPERATOR_NAMESPACE}"/$(basename "${pod}"):/tmp/nvidia-bug-report.log.gz /tmp/nvidia-bug-report.log.gz || \
        (echo "Failed to save nvidia-bug-report from ${pod_nodename}" && continue)

    mv /tmp/nvidia-bug-report.log.gz "${ARTIFACT_DIR}/nvidia-bug-report_${pod_nodename}.log.gz"
done

echo ""
echo "#"
echo "# GPU device usage (nvidia-smi, lsof, fuser, ps)"
echo "#"
echo ""

# Using driver pods list from above
for pod in $driver_pods;
do
    pod_nodename=$(timeout 10s $K get "${pod}" -ojsonpath={.spec.nodeName} -n "${OPERATOR_NAMESPACE}" || echo "unknown")
    echo "Collecting GPU device usage info from ${pod_nodename} ..."

    # Capture nvidia-smi output showing processes using GPUs
    echo "# nvidia-smi output from ${pod_nodename}" > "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"
    timeout 30s $K exec -n "${OPERATOR_NAMESPACE}" "${pod}" -- nvidia-smi >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log" 2>&1 || \
        echo "Failed to run nvidia-smi on ${pod_nodename}" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"

    # Capture lsof output for nvidia devices (run in host namespace)
    echo -e "\n# lsof /run/nvidia/driver/dev/nvidia* output from ${pod_nodename}" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"
    timeout 30s $K exec -n "${OPERATOR_NAMESPACE}" "${pod}" -- nsenter --target 1 --mount --pid -- bash -c 'lsof /run/nvidia/driver/dev/nvidia* 2>&1 || true' >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log" || echo "Timeout running lsof" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"

    # Extract PIDs from lsof output and get detailed process information
    echo -e "\n# Process details for PIDs using GPU devices from ${pod_nodename}" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"
    timeout 60s $K exec -n "${OPERATOR_NAMESPACE}" "${pod}" -- nsenter --target 1 --mount --pid -- bash -c '
        pids=$(lsof /run/nvidia/driver/dev/nvidia* 2>/dev/null | awk "NR>1 {print \$2}" | sort -u)
        if [ -n "$pids" ]; then
            echo "PIDs using GPU: $pids"
            echo ""
            for pid in $pids; do
                echo "=== Process $pid ==="
                ps -p $pid -o pid,ppid,user,stat,start,etime,pcpu,pmem,vsz,rss,args 2>&1 || echo "Process $pid not found"
                echo ""
            done
        else
            echo "No processes found using GPU devices"
        fi
    ' >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log" 2>&1 || true

    # Capture fuser output for nvidia devices (run in host namespace)
    echo -e "\n# fuser -v /run/nvidia/driver/dev/nvidia* output from ${pod_nodename}" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"
    timeout 30s $K exec -n "${OPERATOR_NAMESPACE}" "${pod}" -- nsenter --target 1 --mount --pid -- bash -c 'fuser -v /run/nvidia/driver/dev/nvidia* 2>&1 || true' >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log" || echo "Timeout running fuser" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"

    # List all nvidia device files
    echo -e "\n# ls -la /run/nvidia/driver/dev/nvidia* output from ${pod_nodename}" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"
    timeout 30s $K exec -n "${OPERATOR_NAMESPACE}" "${pod}" -- nsenter --target 1 --mount --pid -- bash -c 'ls -la /run/nvidia/driver/dev/nvidia* 2>&1 || true' >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log" || echo "Timeout running ls" >> "${ARTIFACT_DIR}/gpu_device_usage_${pod_nodename}.log"
done

echo ""
echo "#"
echo "# All done!"
if [[ "$0" != "/usr/bin/gather" ]]; then
    echo "# Logs saved into ${ARTIFACT_DIR}."
fi
echo "#"
