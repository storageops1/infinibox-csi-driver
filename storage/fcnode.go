/*Copyright 2020 Infinidat
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.*/
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "infinibox-csi-driver/helper/logger"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume/util"
)

type fcDevice struct {
	connector *Connector
	disk      string
	isBlock   bool
}

type diskInfo struct {
	MpathDevice string
	IsBlock     bool
	VolName     string
}

type FCMounter struct {
	ReadOnly     bool
	FsType       string
	MountOptions []string
	Mounter      *mount.SafeFormatAndMount
	Exec         mount.Exec
	DeviceUtil   util.DeviceUtil
	TargetPath   string
	StagePath    string
	fcDisk       fcDevice
}

func (fc *fcstorage) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	log.Debugf("NodePublishVolume called")
	fcDetails, err := fc.getFCDiskDetails(req)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}
	switch volCap.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		fcDetails.isBlock = true
	}

	devicePath, err := fc.AttachFCDisk(*fcDetails.connector, &OSioHandler{})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	diskMounter := fc.getFCDiskMounter(req, *fcDetails)
	err = fc.MountFCDisk(diskMounter, devicePath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (fc *fcstorage) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from FC NodeUnpublishVolume  " + fmt.Sprint(res))
		}
	}()
	targetPath := req.GetTargetPath()
	if err := fc.DetachFCDisk(targetPath, &OSioHandler{}); err != nil {
		return nil, err
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (fc *fcstorage) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from FC NodeStageVolume  " + fmt.Sprint(res))
		}
	}()
	log.Info("NodeStageVolume called with ", req.GetPublishContext())
	hostID := req.GetPublishContext()["hostID"]
	ports := req.GetPublishContext()["hostPorts"]

	hstID, _ := strconv.Atoi(hostID)
	log.Debugf("publishing volume to host id is %s", hostID)
	//validate host exists
	if hstID < 1 {
		log.Errorf("hostID %d is not valid host ID", hstID)
		return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, "not a valid host")
	}
	fcPorts := getPortName()
	if len(fcPorts) == 0 {
		log.Error("port name not found on worker")
		return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, "Port name not found")
	}
	for _, fcp := range fcPorts {
		if !strings.Contains(ports, fcp) {
			log.Debugf("host port %s is not created, creating it", fcp)
			err = fc.cs.AddPortForHost(hstID, "FC", fcp)
			if err != nil {
				log.Errorf("error creating host port %v", err)
				return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, err.Error())
			}
			_, err := fc.cs.api.GetHostPort(hstID, fcp)
			if err != nil {
				log.Errorf("failed to get host port %s with error %v", fcp, err)
				return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, err.Error())
			}
		}
	}
	log.Debug("NodeStageVolume completed")
	return &csi.NodeStageVolumeResponse{}, nil
}
func (fc *fcstorage) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	log.Info("Called FC NodeUnstageVolume")
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from FC NodeUnstageVolume  " + fmt.Sprint(res))
		}
	}()
	var mpathDevice string
	stagePath := req.GetStagingTargetPath()

	volproto := strings.Split(req.GetVolumeId(), "$$")
	volName := volproto[0]

	dskInfo := diskInfo{}
	dskInfo.VolName = volName

	// load iscsi disk config from json file
	log.Debug("read fc config from staging path")
	if err := fc.loadFcDiskInfoFromFile(&dskInfo, stagePath); err == nil {
		mpathDevice = dskInfo.MpathDevice
		log.Debugf("fc config: mpathDevice %s", mpathDevice)
	} else {
		log.Debug("fc config not existing at staging path")
		confFile := path.Join("/host", stagePath, volName+".json")
		log.Debug("check if fc config file exists")
		pathExist, pathErr := fc.cs.pathExists(confFile)
		if pathErr == nil {
			if !pathExist {
				log.Debug("fc config file is not exists")
				if err := os.RemoveAll(stagePath); err != nil {
					log.Errorf("fc: failed to remove mount path Error: %v", err)
					return nil, err
				}
				log.Debug("removed stage path: ", stagePath)
				return &csi.NodeUnstageVolumeResponse{}, nil
			}
		}
		log.Warnf("fc detach disk: failed to get fc config from path %s Error: %v", stagePath, err)
	}

	// remove multipath
	var devices []string
	multiPath := false
	dstPath := mpathDevice

	log.Debug("removing mpath")
	if strings.HasPrefix(dstPath, "/host") {
		dstPath = strings.Replace(dstPath, "/host", "", 1)
	}

	log.Debugf("remove multipath device %s", dstPath)
	if strings.HasPrefix(dstPath, "/dev/dm-") {
		multiPath = true
		devices = findSlaveDevicesOnMultipath(dstPath)
	} else {
		// Add single targetPath to devices
		devices = append(devices, dstPath)
	}
	var lastErr error
	for _, device := range devices {
		err := detachDisk(device)
		if err != nil {
			log.Errorf("fc: detachFCDisk failed. device: %v err: %v", device, err)
			lastErr = fmt.Errorf("fc: detach disk failed. device: %v err: %v", device, err)
		}
	}
	if lastErr != nil {
		log.Errorf("fc: last error occurred during detach disk:\n%v", lastErr)
		return nil, lastErr
	}
	if multiPath {
		log.Debug("flush multipath device using multipath -f ", dstPath)
		_, err := fc.cs.ExecuteWithTimeout(4000, "multipath", []string{"-f", dstPath})
		if err != nil {
			if _, e := os.Stat("/host" + dstPath); os.IsNotExist(e) {
				log.Debugf("multipath device %s deleted", dstPath)
			} else {
				log.Errorf("multipath -f %s failed to device with error %v", dstPath, err.Error())
				return nil, err
			}
		}
	}
	log.Debug("Removed multipath sucessfully!")

	if err := os.RemoveAll("/host" + stagePath); err != nil {
		log.Errorf("fc: failed to remove mount path Error: %v", err)
		return nil, err
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (fc *fcstorage) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (
	*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_UNKNOWN,
					},
				},
			},
		},
	}, nil
}

func (fc *fcstorage) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (
	*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{}, nil
}

func (fc *fcstorage) NodeGetVolumeStats(
	ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return &csi.NodeGetVolumeStatsResponse{}, status.Error(codes.Unimplemented, time.Now().String())

}

func (fc *fcstorage) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return &csi.NodeExpandVolumeResponse{}, status.Error(codes.Unimplemented, time.Now().String())
}

// ------------------------------------ Supporting methods  ---------------------------

func (fc *fcstorage) MountFCDisk(fm FCMounter, devicePath string) error {
	notMnt, err := fm.Mounter.IsLikelyNotMountPoint(fm.TargetPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Heuristic determination of mount point failed: %v", err)
	}

	if !notMnt {
		fmt.Printf("fc: %s already mounted", fm.TargetPath)
	}
	if fm.fcDisk.isBlock {
		log.Infof("Block volume will be mount at file %s", fm.TargetPath)
		if fm.ReadOnly {
			return status.Error(codes.Internal, "Read only is not supported for Block Volume")
		}

		if err := os.MkdirAll(filepath.Dir(fm.TargetPath), 0750); err != nil {
			log.Errorf("fc: failed to mkdir %s, error", filepath.Dir(fm.TargetPath))
			return err
		}

		_, err = os.Create("/host/" + fm.TargetPath)
		if err != nil {
			log.Errorf("failed to create target file %q: %v", fm.TargetPath, err)
			return fmt.Errorf("failed to create target file for raw block bind mount: %v", err)
		}
		devicePath = strings.Replace(devicePath, "/host", "", 1)
		options := []string{"bind"}
		options = append(options, "rw")
		if err := fm.Mounter.Mount(devicePath, fm.TargetPath, "", options); err != nil {
			log.Errorf("fc: failed to mount fc volume %s to %s, error %v", devicePath, fm.TargetPath, err)
			return err
		}
		log.Debug("Block volume mounted successfully")
	} else {
		log.Debugf("mount volume to given path %s", fm.TargetPath)
		if err := os.MkdirAll(fm.TargetPath, 0750); err != nil {
			log.Errorf("fc: failed to mkdir %s, error", fm.TargetPath)
			return err
		}
		var options []string

		if fm.ReadOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}
		options = append(options, fm.MountOptions...)
		if err = fm.Mounter.FormatAndMount(devicePath, fm.TargetPath, fm.FsType, options); err != nil {
			return fmt.Errorf("fc: failed to mount fc volume %s [%s] to %s, error %v", devicePath, fm.FsType, fm.TargetPath, err)
		}
	}
	dskinfo := diskInfo{}
	if strings.HasPrefix(devicePath, "/dev/dm-") {
		dskinfo.MpathDevice = devicePath
		dskinfo.IsBlock = fm.fcDisk.isBlock
		dskinfo.VolName = fm.fcDisk.connector.VolumeName
		if err := fc.createFcConfigFile(dskinfo, fm.StagePath); err != nil {
			log.Errorf("fc: failed to save fc config with error: %v", err)
			return err
		}
	}
	return nil
}
func getPortName() []string {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from FC getPortName  " + fmt.Sprint(res))
		}
	}()
	ports := []string{}
	cmd := "cat /sys/class/fc_host/host*/port_name"
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		log.Errorf("Failed to port name with error %v", err)
		return ports
	}
	portName := string(out)
	if portName != "" {
		for _, port := range strings.Split(strings.TrimSuffix(portName, "\n"), "\n") {
			ports = append(ports, strings.Replace(port, "0x", "", 1))
		}
	}
	log.Debugf("fc ports found %v ", ports)
	return ports
}

func (fc *fcstorage) getFCDiskDetails(req *csi.NodePublishVolumeRequest) (*fcDevice, error) {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from FC getFCDiskDetails " + fmt.Sprint(res))
		}
	}()
	volproto := strings.Split(req.GetVolumeId(), "$$")
	volName := volproto[0]
	lun := req.GetPublishContext()["lun"]
	wwids := req.GetVolumeContext()["WWIDs"]
	wwidList := strings.Split(wwids, ",")
	targetList := []string{}
	fcNodes, err := fc.cs.api.GetFCPorts()
	if err != nil {
		return nil, fmt.Errorf("Error getting fiber channel details")
	}
	for _, fcnode := range fcNodes {
		for _, fcport := range fcnode.Ports {
			if fcport.WWPn != "" {
				targetList = append(targetList, strings.Replace(fcport.WWPn, ":", "", -1))
			}
		}
	}
	log.Debugf("lun %s , targetList %v , wwidList %v", lun, targetList, wwidList)
	if lun == "" || (len(targetList) == 0 && len(wwidList) == 0) {
		return nil, fmt.Errorf("FC target information is missing")
	}
	fcConnector := &Connector{
		VolumeName: volName,
		TargetWWNs: targetList,
		WWIDs:      wwidList,
		Lun:        lun,
	}
	//Only pass the connector
	return &fcDevice{
		connector: fcConnector,
	}, nil

}

func (fc *fcstorage) getFCDiskMounter(req *csi.NodePublishVolumeRequest, fcDetails fcDevice) FCMounter {
	fstype := req.GetVolumeContext()["fstype"]
	mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()
	return FCMounter{
		fcDisk:       fcDetails,
		ReadOnly:     false,
		FsType:       fstype,
		MountOptions: mountOptions,
		Mounter:      &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: mount.NewOsExec()},
		Exec:         mount.NewOsExec(),
		DeviceUtil:   util.NewDeviceHandler(util.NewIOHandler()),
		TargetPath:   req.GetTargetPath(),
		StagePath:    req.GetStagingTargetPath(),
	}
}

type ioHandler interface {
	ReadDir(dirname string) ([]os.FileInfo, error)
	Lstat(name string) (os.FileInfo, error)
	EvalSymlinks(path string) (string, error)
	WriteFile(filename string, data []byte, perm os.FileMode) error
}

//Connector provides a struct to hold all of the needed parameters to make our Fibre Channel connection
type Connector struct {
	VolumeName string
	TargetWWNs []string
	Lun        string
	WWIDs      []string
	io         ioHandler
}

//OSioHandler is a wrapper that includes all the necessary io functions used for (Should be used as default io handler)
type OSioHandler struct{}

//ReadDir calls the ReadDir function from ioutil package
func (handler *OSioHandler) ReadDir(dirname string) ([]os.FileInfo, error) {
	return ioutil.ReadDir(dirname)
}

//Lstat calls the Lstat function from os package
func (handler *OSioHandler) Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}

//EvalSymlinks calls EvalSymlinks from filepath package
func (handler *OSioHandler) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

//WriteFile calls WriteFile from ioutil package
func (handler *OSioHandler) WriteFile(filename string, data []byte, perm os.FileMode) error {
	return ioutil.WriteFile(filename, data, perm)
}

// FindMultipathDeviceForDevice given a device name like /dev/sdx, find the devicemapper parent
func (fc *fcstorage) findMultipathDeviceForDevice(device string, io ioHandler) (string, error) {
	log.Debug("In findMultipathDeviceForDevice")
	disk, err := fc.findDeviceForPath(device)
	if err != nil {
		return "", err
	}
	sysPath := "/sys/block/"
	if dirs, err2 := io.ReadDir(sysPath); err2 == nil {
		for _, f := range dirs {
			name := f.Name()
			if strings.HasPrefix(name, "dm-") {
				if _, err1 := io.Lstat(sysPath + name + "/slaves/" + disk); err1 == nil {
					return "/dev/" + name, nil
				}
			}
		}
	} else {
		log.Errorf("failed to find multipath device with error %v", err)
		return "", err2
	}
	log.Debug("multipath not configured")
	return "", nil
}

func (fc *fcstorage) findDeviceForPath(path string) (string, error) {
	log.Debug("In findDeviceForPath")
	devicePath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	// if path /dev/hdX split into "", "dev", "hdX" then we will
	// return just the last part
	devicePath = strings.Replace(devicePath, "/host", "", 1)
	parts := strings.Split(devicePath, "/")
	if len(parts) == 3 && strings.HasPrefix(parts[1], "dev") {
		log.Debug("found device ", parts[2])
		return parts[2], nil
	}
	return "", errors.New("Illegal path for device " + devicePath)
}

func scsiHostRescan(io ioHandler) {
	scsiPath := "/sys/class/scsi_host/"
	if dirs, err := io.ReadDir(scsiPath); err == nil {
		for _, f := range dirs {
			name := scsiPath + f.Name() + "/scan"
			data := []byte("- - -")
			io.WriteFile(name, data, 0666)
		}
	}
}

func (fc *fcstorage) searchDisk(c Connector, io ioHandler) (string, error) {
	log.Debug("In searchDisk")
	var diskIds []string
	var disk string
	var dm string

	if len(c.TargetWWNs) != 0 {
		diskIds = c.TargetWWNs
	} else {
		diskIds = c.WWIDs
	}

	rescaned := false
	for true {

		for _, diskID := range diskIds {
			if len(c.TargetWWNs) != 0 {
				disk, dm = fc.findFcDisk(diskID, c.Lun, io)
			} else {
				disk, dm = fc.getDisksWwids(diskID, io)
			}
			// if multipath device is found, break
			log.Debugf("searchDisk: found disk %s and dm %s", disk, dm)
			if dm != "" {
				break
			}
		}
		// if a dm is found, exit loop
		if rescaned || dm != "" {
			break
		}
		// rescan and search again
		// rescan scsi bus
		log.Debugf("searchDisk rescan scsi host")
		scsiHostRescan(io)
		rescaned = true
	}
	// if no disk matches input wwn and lun, exit
	if disk == "" && dm == "" {
		return "", fmt.Errorf("no fc disk found")
	}

	// if multipath devicemapper device is found, use it; otherwise use raw disk
	if dm != "" {
		log.Debugf("multipath devicemapper device is found")
		return dm, nil
	}
	log.Debugf("multipath devicemapper device not found, using raw disk")
	return disk, nil
}

// find the fc device and device mapper parent
func (fc *fcstorage) findFcDisk(wwn, lun string, io ioHandler) (string, string) {
	log.Debug("In findFcDisk")
	FcPath := "-fc-0x" + wwn + "-lun-" + lun
	DevPath := "/host/dev/disk/by-path/"
	if dirs, err := io.ReadDir(DevPath); err == nil {
		for _, f := range dirs {
			name := f.Name()
			if strings.Contains(name, FcPath) {
				if disk, err1 := io.EvalSymlinks(DevPath + name); err1 == nil {
					if dm, err2 := fc.findMultipathDeviceForDevice(disk, io); err2 == nil {
						return disk, dm
					} else {
						log.Errorf("could not find disk with error %v", err2)
					}
				} else {
					log.Errorf("could not find disk with error %v", err1)
				}

			}
		}
	} else {
		log.Errorf("could not find disk with error %v", err)
	}
	return "", ""
}

func (fc *fcstorage) getDisksWwids(wwid string, io ioHandler) (string, string) {
	FcPath := "scsi-" + wwid
	DevID := "/dev/disk/by-id/"
	if dirs, err := io.ReadDir(DevID); err == nil {
		for _, f := range dirs {
			name := f.Name()
			if name == FcPath {
				disk, err := io.EvalSymlinks(DevID + name)
				if err != nil {
					log.Errorf("fc: failed to find a corresponding disk from symlink[%s], error %v", DevID+name, err)
					return "", ""
				}
				if dm, err1 := fc.findMultipathDeviceForDevice(disk, io); err1 != nil {
					return disk, dm
				}
			}
		}
	}
	log.Errorf("fc: failed to find a disk [%s]", DevID+FcPath)
	return "", ""
}

// Attach attempts to attach a fc volume to a node using the provided Connector info
func (fc *fcstorage) AttachFCDisk(c Connector, io ioHandler) (string, error) {
	if io == nil {
		io = &OSioHandler{}
	}
	log.Infof("Attaching fc volume")
	devicePath, err := fc.searchDisk(c, io)
	if err != nil {
		log.Infof("unable to find disk given WWNN or WWIDs with error %v", err)
		return "", err
	}
	devicePath = strings.Replace(devicePath, "/host", "", 1)
	log.Debugf("Attaching fc volume successful, device path %s", devicePath)

	return devicePath, nil
}

// Detach performs a detach operation on a volume
func (fc *fcstorage) DetachFCDisk(targetPath string, io ioHandler) (err error) {
	log.Infof("Detaching fibre channel volume")
	log.Debugf("Called DetachDisk targetpath: %s", targetPath)
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from FC DetachFCDisk  " + fmt.Sprint(res))
		}
	}()
	if io == nil {
		io = &OSioHandler{}
	}
	mounter := mount.New("")
	mntPath := path.Join("/host", targetPath)
	// unmount volume
	if pathExist, pathErr := fc.cs.pathExists(targetPath); pathErr != nil {
		return fmt.Errorf("Error checking if path exists: %v", pathErr)
	} else if !pathExist {
		if pathExist, _ = fc.cs.pathExists(mntPath); pathErr == nil {
			if !pathExist {
				log.Warnf("Warning: Unmount skipped because path does not exist: %v", targetPath)
				return nil
			}
		}
	}
	if err := mounter.Unmount(targetPath); err != nil {
		if strings.Contains(err.Error(), "not mounted") {
			log.Debug("volume not mounted removing files ", targetPath)
			if err := os.RemoveAll(filepath.Dir("/host" + targetPath)); err != nil {
				log.Errorf("fc: failed to remove mount path Error: %v", err)
			}
			return nil
		}
		log.Errorf("fc detach disk: failed to unmount: %s\nError: %v", targetPath, err)
		return err
	}
	if err := os.RemoveAll(filepath.Dir("/host" + targetPath)); err != nil {
		log.Errorf("fc: failed to remove mount path Error: %v", err)
		return err
	}
	log.Debug("Unmouted volume successfully!")

	return nil
}

func (fc *fcstorage) createFcConfigFile(conf diskInfo, mnt string) error {
	file := path.Join("/host", mnt, conf.VolName+".json")
	fp, err := os.Create(file)
	if err != nil {
		log.Errorf("fc: failed creating persist file with error %v", err)
		return fmt.Errorf("fc: create %s err %s", file, err)
	}
	defer fp.Close()
	encoder := json.NewEncoder(fp)
	if err = encoder.Encode(conf); err != nil {
		log.Errorf("fc: failed creating persist file with error %v", err)
		return fmt.Errorf("fc: encode err: %v", err)
	}
	log.Debugf("fc: created persist config file at path %s", file)
	return nil
}

func (fc *fcstorage) loadFcDiskInfoFromFile(conf *diskInfo, mnt string) error {
	file := path.Join("/host", mnt, conf.VolName+".json")
	fp, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("fc: open %s err %s", file, err)
	}
	defer fp.Close()
	decoder := json.NewDecoder(fp)
	if err = decoder.Decode(conf); err != nil {
		return fmt.Errorf("fc: decode err: %v ", err)
	}
	return nil
}
