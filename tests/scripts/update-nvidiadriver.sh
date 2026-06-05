#!/bin/bash

if [[ "${SKIP_UPDATE}" == "true" ]]; then
    echo "Skipping update: SKIP_UPDATE=${SKIP_UPDATE}"
    exit 0
fi

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
source ${SCRIPT_DIR}/.definitions.sh

# Import the check definitions
source ${SCRIPT_DIR}/checks.sh

NVIDIA_DRIVER_NAME="${NVIDIA_DRIVER_NAME:-e2e-driver}"
DEFAULT_NVIDIA_DRIVER_NAME="${DEFAULT_NVIDIA_DRIVER_NAME:-e2e-default-driver}"
SECOND_DEFAULT_NVIDIA_DRIVER_NAME="${SECOND_DEFAULT_NVIDIA_DRIVER_NAME:-e2e-second-default-driver}"
DEFAULT_NVIDIA_DRIVER_LABEL="nvidia.com/gpu-operator.default-driver"

get_default_nvidiadriver_name() {
    kubectl get nvidiadriver -l "${DEFAULT_NVIDIA_DRIVER_LABEL}=true" -o json |
        jq -r '.items[0].metadata.name // empty'
}

get_default_nvidiadriver_count() {
    kubectl get nvidiadriver -l "${DEFAULT_NVIDIA_DRIVER_LABEL}=true" -o json |
        jq '.items | length'
}

create_nvidiadriver_from() {
    local source_name=$1
    local target_name=$2
    local default_label=${3:-false}

    kubectl get nvidiadriver/"${source_name}" -o json | jq --arg name "${target_name}" --arg default_label "${default_label}" --arg default_driver_label "${DEFAULT_NVIDIA_DRIVER_LABEL}" '
        {
            apiVersion: .apiVersion,
            kind: .kind,
            metadata: {
                name: $name,
                labels: (if $default_label == "true" then {($default_driver_label): "true"} else {} end)
            },
            spec: .spec
        }
    ' | kubectl apply -f -
}

remove_default_label() {
    local driver_name=$1

    kubectl label nvidiadriver/"${driver_name}" "${DEFAULT_NVIDIA_DRIVER_LABEL}-" --overwrite
}

restore_default_label() {
    local driver_name=$1

    kubectl label nvidiadriver/"${driver_name}" "${DEFAULT_NVIDIA_DRIVER_LABEL}=true" --overwrite
}

wait_for_default_nvidiadriver() {
    local expected_name=$1
    local current_time=0

    echo "Waiting for NVIDIADriver/${expected_name} to be the only default"
    while :; do
        default_count=$(get_default_nvidiadriver_count)
        default_name=$(get_default_nvidiadriver_name)
        if [[ "${default_count}" -eq 1 && "${default_name}" == "${expected_name}" ]]; then
            echo "NVIDIADriver/${expected_name} is the only default"
            break
        fi

        if [[ "${current_time}" -gt 120 ]]; then
            echo "timeout reached waiting for NVIDIADriver/${expected_name} to be the only default"
            kubectl get nvidiadriver --show-labels
            exit 1
        fi

        sleep 5
        current_time=$((${current_time} + 5))
    done
}

test_arbitrary_name_default_nvidiadriver() {
    local current_default
    current_default=$(get_default_nvidiadriver_name)
    if [[ -z "${current_default}" ]]; then
        echo "default NVIDIADriver not found"
        kubectl get nvidiadriver --show-labels
        exit 1
    fi

    if [[ "${current_default}" == "${DEFAULT_NVIDIA_DRIVER_NAME}" ]]; then
        wait_for_default_nvidiadriver "${DEFAULT_NVIDIA_DRIVER_NAME}"
        return
    fi

    echo "Moving default label from NVIDIADriver/${current_default} to NVIDIADriver/${DEFAULT_NVIDIA_DRIVER_NAME}"
    remove_default_label "${current_default}"
    if ! create_nvidiadriver_from "${current_default}" "${DEFAULT_NVIDIA_DRIVER_NAME}" true; then
        echo "failed to create NVIDIADriver/${DEFAULT_NVIDIA_DRIVER_NAME}; restoring NVIDIADriver/${current_default} default label before failing"
        restore_default_label "${current_default}"
        exit 1
    fi
    kubectl delete nvidiadriver/"${current_default}"
    wait_for_default_nvidiadriver "${DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_nvidiadriver_owner "${DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_nvidiadriver_daemonsets "${DEFAULT_NVIDIA_DRIVER_NAME}"
}

create_nvidiadriver() {
    local default_name
    default_name=$(get_default_nvidiadriver_name)
    if [[ -z "${default_name}" ]]; then
        echo "default NVIDIADriver not found"
        kubectl get nvidiadriver --show-labels
        exit 1
    fi

    echo "Creating user-defined NVIDIADriver/${NVIDIA_DRIVER_NAME} from NVIDIADriver/${default_name}"
    create_nvidiadriver_from "${default_name}" "${NVIDIA_DRIVER_NAME}" false
}

wait_for_nvidiadriver_owner() {
    local driver_name=$1
    local current_time=0
    local gpu_node_count

    gpu_node_count=$(kubectl get node -l nvidia.com/gpu.present=true --no-headers | wc -l)
    echo "Waiting for ${gpu_node_count} GPU node(s) to be owned by NVIDIADriver/${driver_name}"

    while :; do
        owned_count=$(kubectl get nodes -l "nvidia.com/gpu.present=true,nvidia.com/gpu.driver.owner=${driver_name}" --no-headers | wc -l)
        if [[ "${owned_count}" -eq "${gpu_node_count}" ]]; then
            echo "All GPU nodes are owned by NVIDIADriver/${driver_name}"
            break
        fi

        if [[ "${current_time}" -gt $((60 * 15)) ]]; then
            echo "timeout reached waiting for NVIDIADriver/${driver_name} ownership"
            kubectl get nodes -l nvidia.com/gpu.present=true \
                -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.driver\.owner}{"\n"}{end}'
            exit 1
        fi

        echo "NVIDIADriver/${driver_name} owns ${owned_count}/${gpu_node_count} GPU node(s)"
        sleep 5
        current_time=$((${current_time} + 5))
    done
}

get_nvidiadriver_daemonsets() {
    local driver_name=$1
    kubectl get daemonset -l "app.kubernetes.io/component=nvidia-driver" -n "$TEST_NAMESPACE" -o json |
        jq --arg driver_name "${driver_name}" '.items | map(select(.spec.template.spec.nodeSelector["nvidia.com/gpu.driver.owner"] == $driver_name))'
}

wait_for_nvidiadriver_daemonsets() {
    local driver_name=$1
    local current_time=0

    echo "Waiting for daemonsets owned by NVIDIADriver/${driver_name}"
    while :; do
        daemonset_count=$(get_nvidiadriver_daemonsets "${driver_name}" | jq length)
        if [[ "${daemonset_count}" -gt 0 ]]; then
            echo "Found ${daemonset_count} daemonset(s) owned by NVIDIADriver/${driver_name}"
            break
        fi

        if [[ "${current_time}" -gt $((60 * 15)) ]]; then
            echo "timeout reached waiting for daemonsets owned by NVIDIADriver/${driver_name}"
            kubectl get daemonset -l "app.kubernetes.io/component=nvidia-driver" -n "$TEST_NAMESPACE" -o yaml
            exit 1
        fi

        sleep 5
        current_time=$((${current_time} + 5))
    done
}

test_driver_image_updates() {
    # Update driver image version
    kubectl patch nvidiadriver/"${NVIDIA_DRIVER_NAME}" --type='json' -p='[{"op": "replace", "path": "/spec/version", "value": '"$TARGET_DRIVER_VERSION"'}]'
    if [ "$?" -ne 0 ]; then
        echo "cannot update driver image with version $TARGET_DRIVER_VERSION for driver-daemonset"
        exit 1
    fi

    # Verify update is applied to Driver Daemonset
    local current_time=0
    while :; do
        if get_nvidiadriver_daemonsets "${NVIDIA_DRIVER_NAME}" | jq -e --arg version "${TARGET_DRIVER_VERSION}" 'length > 0 and all(.[]; .spec.template.spec.containers[0].image | contains($version))' >/dev/null; then
            break
        fi

        if [[ "${current_time}" -gt 120 ]]; then
            echo "Image update failed for driver daemonset to version $TARGET_DRIVER_VERSION"
            get_nvidiadriver_daemonsets "${NVIDIA_DRIVER_NAME}"
            exit 1
        fi

        sleep 5
        current_time=$((${current_time} + 5))
    done
    echo "driver daemonset image updated successfully to version $TARGET_DRIVER_VERSION"

    # Delete driver pod to trigger update due to OnDelete policy
    kubectl delete pod -l "app.kubernetes.io/component=nvidia-driver" -n "$TEST_NAMESPACE"

    # Wait for the driver upgrade to transition to "upgrade-done" state
    wait_for_driver_upgrade_done
    
    echo "ensuring that the new driver pods with version $TARGET_DRIVER_VERSION come up successfully"

    check_nvidia_driver_pods_ready

    return 0
}

test_custom_labels_override() {
  if ! kubectl patch nvidiadriver/"${NVIDIA_DRIVER_NAME}" --type='json' -p='[{"op": "add", "path": "/spec/labels", "value": {"cloudprovider": "aws", "platform": "kubernetes"}}]';
  then
    echo "cannot update the labels of the NVIDIADriver resource"
    exit 1
  fi

  # Wait for the operator to update the pod template with new labels
  echo "Waiting for DaemonSet pod template to be updated with new labels..."
  local current_time=0
  while :; do
    if get_nvidiadriver_daemonsets "${NVIDIA_DRIVER_NAME}" | jq -e 'length > 0 and all(.[]; .spec.template.metadata.labels.cloudprovider == "aws" and .spec.template.metadata.labels.platform == "kubernetes")' >/dev/null; then
      break
    fi

    if [[ "${current_time}" -gt 120 ]]; then
      echo "timeout reached waiting for DaemonSet pod template labels"
      get_nvidiadriver_daemonsets "${NVIDIA_DRIVER_NAME}"
      exit 1
    fi

    sleep 5
    current_time=$((${current_time} + 5))
  done

  # Delete driver pod to force recreation with updated labels. Existing pods are not automatically restarted due to the DaemonSet's 'OnDelete` updateStrategy.
  echo "Deleting driver pod to trigger recreation with updated labels..."
  kubectl delete pod -l "app.kubernetes.io/component=nvidia-driver" -n "$TEST_NAMESPACE"

  # Wait for the driver upgrade to transition to "upgrade-done" state
  wait_for_driver_upgrade_done

  check_nvidia_driver_pods_ready

  echo "checking nvidia-driver-daemonset labels"
  labeled_pod_count=$(kubectl get pods -n "$TEST_NAMESPACE" -l "app.kubernetes.io/component=nvidia-driver,cloudprovider=aws,platform=kubernetes" --no-headers | wc -l)
  gpu_node_count=$(kubectl get node -l nvidia.com/gpu.present=true --no-headers | wc -l)
  if [[ "${labeled_pod_count}" -ne "${gpu_node_count}" ]]; then
    echo "Custom labels are missing from one or more NVIDIADriver/${NVIDIA_DRIVER_NAME} pods"
    kubectl get pods -n "$TEST_NAMESPACE" -l "app.kubernetes.io/component=nvidia-driver" --show-labels
    exit 1
  fi
}

assert_nvidiadriver_owner_count() {
    local driver_name=$1
    local gpu_node_count
    local owned_count

    gpu_node_count=$(kubectl get node -l nvidia.com/gpu.present=true --no-headers | wc -l)
    owned_count=$(kubectl get nodes -l "nvidia.com/gpu.present=true,nvidia.com/gpu.driver.owner=${driver_name}" --no-headers | wc -l)
    if [[ "${owned_count}" -ne "${gpu_node_count}" ]]; then
        echo "Expected ${gpu_node_count} GPU node(s) to remain owned by NVIDIADriver/${driver_name}, found ${owned_count}"
        kubectl get nodes -l nvidia.com/gpu.present=true \
            -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.driver\.owner}{"\n"}{end}'
        exit 1
    fi
}

wait_for_nvidiadriver_condition_message() {
    local driver_name=$1
    local message=$2
    local current_time=0

    echo "Waiting for NVIDIADriver/${driver_name} status message to contain: ${message}"
    while :; do
        if kubectl get nvidiadriver/"${driver_name}" -o json | jq -e --arg message "${message}" '
            (.status.state // "") == "notReady" and
            ([.status.conditions[]?.message // ""] | any(contains($message)))
        ' >/dev/null; then
            break
        fi

        if [[ "${current_time}" -gt 120 ]]; then
            echo "timeout reached waiting for NVIDIADriver/${driver_name} status message"
            kubectl get nvidiadriver/"${driver_name}" -o yaml
            exit 1
        fi

        sleep 5
        current_time=$((${current_time} + 5))
    done
}

wait_for_clusterpolicy_condition_message() {
    local message=$1
    local current_time=0

    echo "Waiting for ClusterPolicy/cluster-policy status message to contain: ${message}"
    while :; do
        if kubectl get clusterpolicy/cluster-policy -o json | jq -e --arg message "${message}" '
            (.status.state // "") == "notReady" and
            ([.status.conditions[]?.message // ""] | any(contains($message)))
        ' >/dev/null; then
            break
        fi

        if [[ "${current_time}" -gt 120 ]]; then
            echo "timeout reached waiting for ClusterPolicy/cluster-policy status message"
            kubectl get clusterpolicy/cluster-policy -o yaml
            exit 1
        fi

        sleep 5
        current_time=$((${current_time} + 5))
    done
}

test_removed_default_label_conflict_preserves_owners() {
    echo "Testing that removing the default label makes the CR a normal conflicting NVIDIADriver"
    remove_default_label "${DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_nvidiadriver_condition_message "${NVIDIA_DRIVER_NAME}" "conflicting NVIDIADriver NodeSelectors found"
    wait_for_nvidiadriver_condition_message "${DEFAULT_NVIDIA_DRIVER_NAME}" "conflicting NVIDIADriver NodeSelectors found"
    wait_for_clusterpolicy_condition_message "conflicting NVIDIADriver NodeSelectors found"
    assert_nvidiadriver_owner_count "${NVIDIA_DRIVER_NAME}"
    restore_default_label "${DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_default_nvidiadriver "${DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_nvidiadriver_owner "${NVIDIA_DRIVER_NAME}"
}

test_multiple_default_labels() {
    echo "Testing multiple default-labeled NVIDIADrivers"
    if ! create_nvidiadriver_from "${DEFAULT_NVIDIA_DRIVER_NAME}" "${SECOND_DEFAULT_NVIDIA_DRIVER_NAME}" true; then
        echo "creation of second default-labeled NVIDIADriver was rejected"
        return
    fi

    wait_for_nvidiadriver_condition_message "${SECOND_DEFAULT_NVIDIA_DRIVER_NAME}" "multiple default NVIDIADrivers found"
    assert_nvidiadriver_owner_count "${NVIDIA_DRIVER_NAME}"
    kubectl delete nvidiadriver/"${SECOND_DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_default_nvidiadriver "${DEFAULT_NVIDIA_DRIVER_NAME}"
    wait_for_nvidiadriver_owner "${NVIDIA_DRIVER_NAME}"
}

test_arbitrary_name_default_nvidiadriver
create_nvidiadriver
wait_for_nvidiadriver_owner "${NVIDIA_DRIVER_NAME}"
wait_for_nvidiadriver_daemonsets "${NVIDIA_DRIVER_NAME}"
check_nvidia_driver_pods_ready
test_driver_image_updates
test_custom_labels_override
test_removed_default_label_conflict_preserves_owners
test_multiple_default_labels
