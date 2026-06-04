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

create_nvidiadriver() {
    echo "Creating user-defined NVIDIADriver/${NVIDIA_DRIVER_NAME} from NVIDIADriver/default"
    kubectl get nvidiadriver/default -o json | jq --arg name "${NVIDIA_DRIVER_NAME}" '
        {
            apiVersion: .apiVersion,
            kind: .kind,
            metadata: {
                name: $name
            },
            spec: .spec
        }
    ' | kubectl apply -f -
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
    kubectl patch nvidiadriver/"${NVIDIA_DRIVER_NAME}" --type='merge' -p="{\"spec\":{\"version\":\"${TARGET_DRIVER_VERSION}\"}}"
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
  if ! kubectl patch nvidiadriver/"${NVIDIA_DRIVER_NAME}" --type='merge' -p='{"spec":{"labels":{"cloudprovider":"aws","platform":"kubernetes"}}}';
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

create_nvidiadriver
wait_for_nvidiadriver_owner "${NVIDIA_DRIVER_NAME}"
wait_for_nvidiadriver_daemonsets "${NVIDIA_DRIVER_NAME}"
check_nvidia_driver_pods_ready
test_driver_image_updates
test_custom_labels_override
