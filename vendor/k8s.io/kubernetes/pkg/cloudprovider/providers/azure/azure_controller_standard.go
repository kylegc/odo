/*
Copyright 2018 The Kubernetes Authors.

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

package azure

import (
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-03-01/compute"
	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/types"
)

// AttachDisk attaches a vhd to vm
// the vhd must exist, can be identified by diskName, diskURI, and lun.
func (as *availabilitySet) AttachDisk(isManagedDisk bool, diskName, diskURI string, nodeName types.NodeName, lun int32, cachingMode compute.CachingTypes) error {
	vm, err := as.getVirtualMachine(nodeName)
	if err != nil {
		return err
	}

	disks := *vm.StorageProfile.DataDisks
	if isManagedDisk {
		disks = append(disks,
			compute.DataDisk{
				Name:         &diskName,
				Lun:          &lun,
				Caching:      cachingMode,
				CreateOption: "attach",
				ManagedDisk: &compute.ManagedDiskParameters{
					ID: &diskURI,
				},
			})
	} else {
		disks = append(disks,
			compute.DataDisk{
				Name: &diskName,
				Vhd: &compute.VirtualHardDisk{
					URI: &diskURI,
				},
				Lun:          &lun,
				Caching:      cachingMode,
				CreateOption: "attach",
			})
	}

	newVM := compute.VirtualMachine{
		Location: vm.Location,
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			HardwareProfile: vm.HardwareProfile,
			StorageProfile: &compute.StorageProfile{
				DataDisks: &disks,
			},
		},
	}
	vmName := mapNodeNameToVMName(nodeName)
	glog.V(2).Infof("azureDisk - update(%s): vm(%s) - attach disk(%s, %s)", as.resourceGroup, vmName, diskName, diskURI)
	ctx, cancel := getContextWithCancel()
	defer cancel()

	// Invalidate the cache right after updating
	defer as.cloud.vmCache.Delete(vmName)

	_, err = as.VirtualMachinesClient.CreateOrUpdate(ctx, as.resourceGroup, vmName, newVM)
	if err != nil {
		glog.Errorf("azureDisk - attach disk(%s, %s) failed, err: %v", diskName, diskURI, err)
		detail := err.Error()
		if strings.Contains(detail, errLeaseFailed) || strings.Contains(detail, errDiskBlobNotFound) {
			// if lease cannot be acquired or disk not found, immediately detach the disk and return the original error
			glog.V(2).Infof("azureDisk - err %v, try detach disk(%s, %s)", err, diskName, diskURI)
			as.DetachDiskByName(diskName, diskURI, nodeName)
		}
	} else {
		glog.V(2).Infof("azureDisk - attach disk(%s, %s) succeeded", diskName, diskURI)
	}
	return err
}

// DetachDiskByName detaches a vhd from host
// the vhd can be identified by diskName or diskURI
func (as *availabilitySet) DetachDiskByName(diskName, diskURI string, nodeName types.NodeName) error {
	vm, err := as.getVirtualMachine(nodeName)
	if err != nil {
		// if host doesn't exist, no need to detach
		glog.Warningf("azureDisk - cannot find node %s, skip detaching disk %s", nodeName, diskName)
		return nil
	}

	disks := *vm.StorageProfile.DataDisks
	bFoundDisk := false
	for i, disk := range disks {
		if disk.Lun != nil && (disk.Name != nil && diskName != "" && *disk.Name == diskName) ||
			(disk.Vhd != nil && disk.Vhd.URI != nil && diskURI != "" && *disk.Vhd.URI == diskURI) ||
			(disk.ManagedDisk != nil && diskURI != "" && *disk.ManagedDisk.ID == diskURI) {
			// found the disk
			glog.V(2).Infof("azureDisk - detach disk: name %q uri %q", diskName, diskURI)
			disks = append(disks[:i], disks[i+1:]...)
			bFoundDisk = true
			break
		}
	}

	if !bFoundDisk {
		return fmt.Errorf("detach azure disk failure, disk %s not found, diskURI: %s", diskName, diskURI)
	}

	newVM := compute.VirtualMachine{
		Location: vm.Location,
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			HardwareProfile: vm.HardwareProfile,
			StorageProfile: &compute.StorageProfile{
				DataDisks: &disks,
			},
		},
	}
	vmName := mapNodeNameToVMName(nodeName)
	glog.V(2).Infof("azureDisk - update(%s): vm(%s) - detach disk(%s, %s)", as.resourceGroup, vmName, diskName, diskURI)
	ctx, cancel := getContextWithCancel()
	defer cancel()

	// Invalidate the cache right after updating
	defer as.cloud.vmCache.Delete(vmName)

	resp, err := as.VirtualMachinesClient.CreateOrUpdate(ctx, as.resourceGroup, vmName, newVM)
	if as.CloudProviderBackoff && shouldRetryHTTPRequest(resp, err) {
		glog.V(2).Infof("azureDisk - update(%s) backing off: vm(%s) detach disk(%s, %s), err: %v", as.resourceGroup, vmName, diskName, diskURI, err)
		retryErr := as.CreateOrUpdateVMWithRetry(vmName, newVM)
		if retryErr != nil {
			err = retryErr
			glog.V(2).Infof("azureDisk - update(%s) abort backoff: vm(%s) detach disk(%s, %s), err: %v", as.resourceGroup, vmName, diskName, diskURI, err)
		}
	}
	if err != nil {
		glog.Errorf("azureDisk - detach disk(%s, %s) failed, err: %v", diskName, diskURI, err)
	} else {
		glog.V(2).Infof("azureDisk - detach disk(%s, %s) succeeded", diskName, diskURI)
	}

	return err
}

// GetDataDisks gets a list of data disks attached to the node.
func (as *availabilitySet) GetDataDisks(nodeName types.NodeName) ([]compute.DataDisk, error) {
	vm, err := as.getVirtualMachine(nodeName)
	if err != nil {
		return nil, err
	}

	if vm.StorageProfile.DataDisks == nil {
		return nil, nil
	}

	return *vm.StorageProfile.DataDisks, nil
}
