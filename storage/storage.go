// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: GPL-3.0-only

package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/clearlinux/clr-installer/cmd"
	"github.com/clearlinux/clr-installer/errors"
	"github.com/clearlinux/clr-installer/log"
	"github.com/clearlinux/clr-installer/utils"
)

// PartedPartition hold partition information
// Number 0 and FileSystem "free" are free spaces
type PartedPartition struct {
	Number     uint64 // partition number 0 indicates free space
	Start      uint64 // starting byte location
	End        uint64 // ending byte location
	Size       uint64 // size in bytes
	FileSystem string // file system Type
	Name       string // partition name
	Flags      string // flags for partition
}

// A BlockDevice describes a block device and its partitions
type BlockDevice struct {
	Name            string             // device name
	MappedName      string             // mapped device name
	Model           string             // device model
	MajorMinor      string             // major:minor device number
	PtType          string             // partition table type
	FsType          string             // filesystem type
	UUID            string             // filesystem uuid
	Serial          string             // device serial number
	MountPoint      string             // where the device is mounted
	Label           string             // label for the partition; set with mkfs
	Size            uint64             // size of the device
	Type            BlockDeviceType    // device type
	State           BlockDeviceState   // device state (running, live etc)
	ReadOnly        bool               // read-only device
	RemovableDevice bool               // removable device
	Children        []*BlockDevice     // children devices/partitions
	Parent          *BlockDevice       // Parent block device; nil for disk
	UserDefined     bool               // was this value set by user?
	MakePartition   bool               // Do we need to make a new partition?
	FormatPartition bool               // Do we need to format the partition
	Options         string             // arbitrary mkfs.* options
	available       bool               // was it mounted the moment we loaded?
	partition       uint64             // Assigned partition for media - can't set until after mkpart
	PartTable       []*PartedPartition // Existing Disk partition table from parted
	removedParts    []uint64           // List of manually removed partitions
}

// Version used for reading and writing YAML
type blockDeviceYAMLMarshal struct {
	Name            string         `yaml:"name,omitempty"`
	Model           string         `yaml:"model,omitempty"`
	MajorMinor      string         `yaml:"majMin,omitempty"`
	FsType          string         `yaml:"fstype,omitempty"`
	UUID            string         `yaml:"uuid,omitempty"`
	Serial          string         `yaml:"serial,omitempty"`
	MountPoint      string         `yaml:"mountpoint,omitempty"`
	Label           string         `yaml:"label,omitempty"`
	Size            string         `yaml:"size,omitempty"`
	ReadOnly        string         `yaml:"ro,omitempty"`
	RemovableDevice string         `yaml:"rm,omitempty"`
	Type            string         `yaml:"type,omitempty"`
	State           string         `yaml:"state,omitempty"`
	Children        []*BlockDevice `yaml:"children,omitempty"`
	Options         string         `yaml:"options,omitempty"`
}

// BlockDeviceState is the representation of a block device state (live, running, etc)
type BlockDeviceState int

// BlockDeviceType is the representation of a block device type (disk, part, rom, etc)
type BlockDeviceType int

const (
	// BlockDeviceTypeDisk identifies a BlockDevice as a disk
	BlockDeviceTypeDisk = iota

	// BlockDeviceTypePart identifies a BlockDevice as a partition
	BlockDeviceTypePart

	// BlockDeviceTypeRom identifies a BlockDevice as a rom
	BlockDeviceTypeRom

	// BlockDeviceTypeLVM2Group identifies a BlockDevice as a lvm2 group
	BlockDeviceTypeLVM2Group

	// BlockDeviceTypeLVM2Volume identifies a BlockDevice as a lvm2 volume
	BlockDeviceTypeLVM2Volume

	// BlockDeviceTypeCrypt identifies a BlockDevice as an encrypted partition (created with cryptsetup)
	BlockDeviceTypeCrypt

	// BlockDeviceTypeLoop identifies a BlockDevice as a loop device (created with losetup)
	BlockDeviceTypeLoop

	// BlockDeviceTypeUnknown identifies a BlockDevice as unknown
	BlockDeviceTypeUnknown

	// BlockDeviceStateUnknown identifies a BlockDevice in a unknown state
	BlockDeviceStateUnknown = iota

	// BlockDeviceStateRunning identifies a BlockDevice as running
	BlockDeviceStateRunning

	// BlockDeviceStateLive identifies a BlockDevice as live
	BlockDeviceStateLive

	// BlockDeviceStateConnected identifies a BlockDevice as Connected
	BlockDeviceStateConnected

	// MinimumPartitionSize is smallest size for any partition
	MinimumPartitionSize = 1048576

	// SafeWholeWarning specifies the warning message for whole disk partition
	SafeWholeWarning = "Selected media will be partitioned."

	// SafePartialWarning specifies the warning message for partial disk partition
	SafePartialWarning = "Selected media will have partitions added."

	// DestructiveWarning specifies the warning message for destructive installation
	DestructiveWarning = "WARNING: Selected media will be erased."

	// DataLossWarning specifies the warning message for data loss installation
	DataLossWarning = "WARNING: Selected media will have data loss."

	// ConfirmInstallation specifies the installation warning title
	ConfirmInstallation = "Confirm Installation"

	// EncryptionPassphrase specifies the title for encryption passphrase dialog
	EncryptionPassphrase = "Encryption Passphrase"

	// PassphraseMessage specifies the text for encryption passphrase dialog
	PassphraseMessage = "Encryption requires a Passphrase"
)

var (
	avBlockDevices      []*BlockDevice
	lsblkBinary         = "lsblk"
	storageExp          = regexp.MustCompile(`^([0-9]*(\.)?[0-9]*)([bkmgtp]{1}){0,1}$`)
	labelExp            = regexp.MustCompile(`^([[:word:]-+_]+)$`)
	mountExp            = regexp.MustCompile(`^(/|(/[[:word:]-+_]+)+)$`)
	devNameSuffixExp    = regexp.MustCompile(`([0-9]*)$`)
	blockDeviceStateMap = map[BlockDeviceState]string{
		BlockDeviceStateRunning:   "running",
		BlockDeviceStateLive:      "live",
		BlockDeviceStateConnected: "Connected",
		BlockDeviceStateUnknown:   "",
	}
	blockDeviceTypeMap = map[BlockDeviceType]string{
		BlockDeviceTypeDisk:       "disk",
		BlockDeviceTypePart:       "part",
		BlockDeviceTypeCrypt:      "crypt",
		BlockDeviceTypeLoop:       "loop",
		BlockDeviceTypeRom:        "rom",
		BlockDeviceTypeLVM2Group:  "LVM2_member",
		BlockDeviceTypeLVM2Volume: "lvm",
		BlockDeviceTypeUnknown:    "",
	}
	aliasPrefixTable = map[string]string{
		"/dev/loop":   "p",
		"/dev/nvme":   "p",
		"/dev/mmcblk": "p",
	}

	bootSize = uint64(150 * (1000 * 1000))
	swapSize = uint64(256 * (1000 * 1000))
)

func getAliasSuffix(file string) string {
	for k, v := range aliasPrefixTable {
		if strings.HasPrefix(file, k) {
			return v
		}
	}

	return ""
}

// ExpandName expands variables in the Name attribute applying the values in the
// alias map
func (bd *BlockDevice) ExpandName(alias map[string]string) {
	tmap := map[string]string{}

	bd.Name = utils.ExpandVariables(alias, bd.Name)

	for k, v := range alias {
		tmap[k] = fmt.Sprintf("%s%s", v, getAliasSuffix(filepath.Join("/dev", v)))
	}

	for _, child := range bd.Children {
		child.Name = utils.ExpandVariables(tmap, child.Name)
	}
}

// GetNewPartitionName returns the name with the new partition number
func (bd *BlockDevice) GetNewPartitionName(partition uint64) string {
	// Replace the last set of digits with the current partition number
	return devNameSuffixExp.ReplaceAllString(bd.getBasePartitionName(), fmt.Sprintf("%d", partition))
}

// SetPartitionNumber is set when we add a new partition to a disk
// which stores the newly allocated partition number, and then corrects
// the devices partition name
func (bd *BlockDevice) SetPartitionNumber(partition uint64) {
	bd.partition = partition
}

// GetDeviceFile formats the block device's file path
func (bd BlockDevice) GetDeviceFile() string {
	return filepath.Join("/dev/", bd.Name)
}

// GetMappedDeviceFile formats the block device's file path
// using the mapped device name
func (bd BlockDevice) GetMappedDeviceFile() string {

	if bd.MappedName != "" {
		return filepath.Join("/dev/", bd.MappedName)
	}

	return filepath.Join("/dev/", bd.Name)
}

// GetDeviceID returns an identifier for the block device
// First trying, label, then UUID, then finally the raw device
// String is suitable for the /etc/fstab
func (bd BlockDevice) GetDeviceID() string {
	if bd.Label != "" {
		return "LABEL=" + bd.Label
	}

	if bd.UUID != "" {
		return "UUID=" + bd.UUID
	}

	return bd.GetDeviceFile()
}

func (bt BlockDeviceType) String() string {
	return blockDeviceTypeMap[bt]
}

func parseBlockDeviceType(bdt string) (BlockDeviceType, error) {
	for k, v := range blockDeviceTypeMap {
		if v == bdt {
			return k, nil
		}
	}

	return BlockDeviceTypeUnknown, errors.Errorf("Unknown block device type: %s", bdt)
}

func (bs BlockDeviceState) String() string {
	return blockDeviceStateMap[bs]
}

func parseBlockDeviceState(bds string) (BlockDeviceState, error) {
	for k, v := range blockDeviceStateMap {
		if v == bds {
			return k, nil
		}
	}

	return BlockDeviceStateUnknown, errors.Errorf("Unrecognized block device state: %s", bds)
}

func (bd *BlockDevice) findFree(size uint64) *PartedPartition {
	var freePart *PartedPartition

	for _, part := range bd.PartTable {
		if part.Number == 0 && part.FileSystem == "free" {
			if part.Size >= size {
				freePart = part.Clone()
				break
			}
		}
	}

	return freePart
}

// Clone creates a copies a PartedPartition
func (part *PartedPartition) Clone() *PartedPartition {
	clone := &PartedPartition{
		Number:     part.Number,
		Start:      part.Start,
		End:        part.End,
		Size:       part.Size,
		FileSystem: part.FileSystem,
		Name:       part.Name,
		Flags:      part.Flags,
	}

	return clone
}

// Clone creates a copies a BlockDevice and its children
func (bd *BlockDevice) Clone() *BlockDevice {
	clone := &BlockDevice{
		Name:            bd.Name,
		MappedName:      bd.MappedName,
		Model:           bd.Model,
		MajorMinor:      bd.MajorMinor,
		FsType:          bd.FsType,
		UUID:            bd.UUID,
		Serial:          bd.Serial,
		MountPoint:      bd.MountPoint,
		Label:           bd.Label,
		Size:            bd.Size,
		Type:            bd.Type,
		State:           bd.State,
		ReadOnly:        bd.ReadOnly,
		RemovableDevice: bd.RemovableDevice,
		Parent:          bd.Parent,
		UserDefined:     bd.UserDefined,
		MakePartition:   bd.MakePartition,
		FormatPartition: bd.FormatPartition,
		available:       bd.available,
		partition:       bd.partition,
		PartTable:       bd.PartTable,
		removedParts:    bd.removedParts,
	}

	clone.Children = []*BlockDevice{}

	for _, curr := range bd.Children {
		cc := curr.Clone()
		cc.Parent = clone

		clone.Children = append(clone.Children, cc)
	}

	return clone
}

// IsUserDefined returns true if the configuration was interactively
// defined by the user
func (bd *BlockDevice) IsUserDefined() bool {
	return bd.UserDefined
}

// IsAvailable returns true if the media is not a installer media, returns false otherwise
func (bd *BlockDevice) IsAvailable() bool {
	return bd.available
}

// ConfigStatus is the status type for Configuration Status of the installation media
type ConfigStatus int

const (
	// ConfiguredNone indicates no installation media has been configured
	ConfiguredNone ConfigStatus = iota
	// ConfiguredPartial indicates only some of the required partition are configured
	ConfiguredPartial
	// ConfiguredEntire indicates ALL of the required partition are configured
	ConfiguredEntire
)

// GetConfiguredStatus check a block device for the required partitions
// Returns either ConfiguredNone,ConfiguredPartial, or ConfiguredEntire
func (bd *BlockDevice) GetConfiguredStatus() ConfigStatus {
	status := ConfiguredNone

	var root, boot, swap bool
	for _, part := range bd.Children {

		if part.FsType == "vfat" && part.MountPoint == "/boot" {
			boot = true
		}
		if part.MountPoint == "/" {
			root = true
		}
		if part.FsType == "swap" {
			swap = true
		}
	}
	if boot || root {
		status = ConfiguredPartial
	}
	if boot && root && swap {
		status = ConfiguredEntire
	}

	return status
}

// FsTypeNotSwap returns true if the file system type is not swap
func (bd *BlockDevice) FsTypeNotSwap() bool {
	return bd.FsType != "swap"
}

// DeviceHasSwap returns true if the block device has a swap partition
func (bd *BlockDevice) DeviceHasSwap() bool {
	hasSwap := false

	for _, part := range bd.Children {
		if part.FsType == "swap" {
			hasSwap = true
		}
	}
	return hasSwap
}

// Validate checks if the minimal requirements for a installation is met
func (bd *BlockDevice) Validate(legacyBios bool, cryptPass string) error {
	bootPartition := false
	rootPartition := false
	encrypted := false

	for _, ch := range bd.Children {
		if ch.FsType == "vfat" && ch.MountPoint == "/boot" {
			bootPartition = true

			if ch.Type == BlockDeviceTypeCrypt {
				return errors.Errorf("Encryption of /boot is not supported")
			}
		}

		if ch.MountPoint == "/" {
			rootPartition = true
		}

		if ch.Type == BlockDeviceTypeCrypt && ch.FsTypeNotSwap() {
			encrypted = true
		}

		if bd.Type != BlockDeviceTypeDisk && bd.Size == 0 && ch.Size == 0 {
			return errors.Errorf("Both image size and partition size cannot be 0")
		}
	}

	if !bootPartition && !legacyBios {
		return errors.Errorf("Could not find a suitable EFI partition")
	}

	if !rootPartition {
		return errors.Errorf("Could not find a root partition")
	}

	if encrypted && cryptPass == "" {
		return errors.Errorf("Encrypted file system enabled, but missing passphase")
	}

	return nil
}

// RemoveChild removes a partition from disk block device
func (bd *BlockDevice) RemoveChild(child *BlockDevice) {
	copyBd := bd.Clone()

	bd.Children = nil

	for _, curr := range copyBd.Children {
		if curr.Name == child.Name {
			child.Parent = nil
			continue
		}

		curr.Name = ""
		bd.AddChild(curr)
	}
}

// addRemovePartition adds a partition to the list to be removed
func (bd *BlockDevice) addRemovePartition(part uint64) {
	bd.removedParts = append(bd.removedParts, part)
}

func (bd *BlockDevice) getBasePartitionName() string {
	partPrefix := ""

	if bd.Type == BlockDeviceTypeLoop ||
		strings.Contains(bd.Name, "nvme") ||
		strings.Contains(bd.Name, "mmcblk") {
		partPrefix = "p"
	}

	return fmt.Sprintf("%s%s", bd.Name, partPrefix)
}

// AddChild adds a partition to a disk block device
func (bd *BlockDevice) AddChild(child *BlockDevice) {
	if bd.Children == nil {
		bd.Children = []*BlockDevice{}
	}

	child.Parent = bd
	bd.Children = append(bd.Children, child)

	if child.Name == "" {
		if child.partition < 1 {
			child.Name = fmt.Sprintf("%s?", bd.getBasePartitionName())
		} else {
			child.Name = fmt.Sprintf("%s%d", bd.getBasePartitionName(), child.partition)
		}
	}
	log.Debug("AddChild: child.Name is %q", child.Name)
}

// HumanReadableSizeWithUnitAndPrecision converts the size representation in bytes to the
// closest human readable format i.e 10M, 1G, 2T etc with a forced unit and precision
func HumanReadableSizeWithUnitAndPrecision(size uint64, unit string, precision int) (string, error) {
	unit = strings.ToUpper(unit)

	if size == 0 {
		return fmt.Sprintf("0"), nil
	}

	sizes := []struct {
		unit      string
		mask      float64
		precision int
	}{
		{"P", 1.0 * 1000.0 * 1000.0 * 1000.0 * 1000.0 * 1000.0, 5},
		{"T", 1.0 * 1000.0 * 1000.0 * 1000.0 * 1000.0, 4},
		{"G", 1.0 * 1000.0 * 1000.0 * 1000.0, 3},
		{"M", 1.0 * 1000.0 * 1000.0, 2},
		{"K", 1.0 * 1000.0, 1},
		{"B", 1.0, 0},
	}

	value := float64(size)
	for _, curr := range sizes {
		csize := value / curr.mask

		// No unit request, use default based on size
		if unit == "" {
			if csize < 1 {
				continue
			}
		} else if unit != curr.unit {
			continue
		}

		unit = curr.unit

		// No precision request, use default based on size
		if precision < 0 {
			precision = curr.precision
		}

		formatted := strconv.FormatFloat(csize, 'f', precision, 64)
		// Remove trailing zeroes (and unused decimal)
		formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
		if unit != "" && unit != "B" {
			formatted += unit
		}

		return formatted, nil
	}

	return "", errors.Errorf("Could not format disk/partition size")
}

// HumanReadableSizeWithPrecision converts the size representation in bytes to the
// closest human readable format i.e 10M, 1G, 2T etc with a forced precision
func HumanReadableSizeWithPrecision(size uint64, precision int) (string, error) {
	return HumanReadableSizeWithUnitAndPrecision(size, "", precision)
}

// HumanReadableSizeWithUnit converts the size representation in bytes to the
// closest human readable format i.e 10M, 1G, 2T etc with a forced unit
func HumanReadableSizeWithUnit(size uint64, unit string) (string, error) {
	return HumanReadableSizeWithUnitAndPrecision(size, unit, -1)
}

// HumanReadableSize converts the size representation in bytes to the closest
// human readable format i.e 10M, 1G, 2T etc
func HumanReadableSize(size uint64) (string, error) {
	return HumanReadableSizeWithUnitAndPrecision(size, "", -1)
}

// FreeSpace returns the block device available/free space considering the currently
// configured partition table
func (bd *BlockDevice) FreeSpace() (uint64, error) {
	if !utils.IntSliceContains([]int{BlockDeviceTypeDisk, BlockDeviceTypeLoop}, int(bd.Type)) {
		return 0, errors.Errorf("FreeSpace() must only be called with a disk block device")
	}

	var total uint64
	for _, curr := range bd.Children {
		total = total + curr.Size
	}

	return bd.Size - total, nil
}

// HumanReadableSizeWithUnitAndPrecision converts the size representation in bytes to the
// closest human readable format i.e 10M, 1G, 2T etc with a forced unit and precision
func (bd *BlockDevice) HumanReadableSizeWithUnitAndPrecision(unit string, precision int) (string, error) {
	return HumanReadableSizeWithUnitAndPrecision(bd.Size, unit, precision)
}

// HumanReadableSizeWithPrecision converts the size representation in bytes to the
// closest human readable format i.e 10M, 1G, 2T etc with a forced precision
func (bd *BlockDevice) HumanReadableSizeWithPrecision(precision int) (string, error) {
	return bd.HumanReadableSizeWithUnitAndPrecision("", precision)
}

// HumanReadableSizeWithUnit converts the size representation in bytes to the
// closest human readable format i.e 10M, 1G, 2T etc with a forced unit
func (bd *BlockDevice) HumanReadableSizeWithUnit(unit string) (string, error) {
	return bd.HumanReadableSizeWithUnitAndPrecision(unit, -1)
}

// HumanReadableSize converts the size representation in bytes to the closest
// human readable format i.e 10M, 1G, 2T etc
func (bd *BlockDevice) HumanReadableSize() (string, error) {
	return bd.HumanReadableSizeWithUnitAndPrecision("", -1)
}

func listBlockDevices(userDefined []*BlockDevice) ([]*BlockDevice, error) {
	w := bytes.NewBuffer(nil)

	args := []string{"partprobe", "-s"}
	err := cmd.RunAndLog(args...)
	if err != nil {
		log.Warning("PartProbe has non-zero exit status: %s", err)
	}

	// Exclude memory(1), floppy(2), and SCSI CDROM(11) devices
	err = cmd.Run(w, lsblkBinary, "--exclude", "1,2,11", "-J", "-b", "-O")
	if err != nil {
		return nil, fmt.Errorf("%s", w.String())
	}

	bds, err := parseBlockDevicesDescriptor(w.Bytes())
	if err != nil {
		return nil, err
	}

	for _, bd := range bds {
		// Read the partition table for the device
		partTable := bd.getPartitionTable()
		bd.setPartitionTable(partTable)
	}

	if userDefined == nil || len(userDefined) == 0 {
		return bds, nil
	}

	merged := []*BlockDevice{}
	for _, loaded := range bds {
		added := false

		for _, udef := range userDefined {
			if !loaded.Equals(udef) {
				continue
			}

			merged = append(merged, udef)
			added = true
			break
		}

		if !added {
			merged = append(merged, loaded)
		}
	}

	return merged, nil
}

// UpdateBlockDevices updates the Label and UUID information only
// for existing available block devices
func UpdateBlockDevices(medias []*BlockDevice) error {

	bds, err := listBlockDevices(nil)
	if err != nil {
		return err
	}

	// Loop though all used media devices and update
	// the labels and uuid
	for _, media := range medias {
		updateBlockDevices(media, bds)
	}

	return nil
}

func updateBlockDevices(toBeUpdated *BlockDevice, updates []*BlockDevice) {
	for _, update := range updates {
		if toBeUpdated.Name == update.Name {
			if toBeUpdated.Children == nil {
				toBeUpdated.Label = update.Label
				toBeUpdated.UUID = update.UUID
				return
			}

			for _, child := range toBeUpdated.Children {
				updateBlockDevices(child, update.Children)
			}
		}
	}
}

// RescanBlockDevices clears current list available block devices and rescans
func RescanBlockDevices(userDefined []*BlockDevice) ([]*BlockDevice, error) {
	avBlockDevices = nil

	return ListAvailableBlockDevices(userDefined)
}

// ListAvailableBlockDevices Lists only available block devices
// where available means block devices not mounted or not in use by the host system
// userDefined will be inserted in the resulting list rather the loaded ones
func ListAvailableBlockDevices(userDefined []*BlockDevice) ([]*BlockDevice, error) {
	if avBlockDevices != nil {
		return avBlockDevices, nil
	}

	bds, err := listBlockDevices(userDefined)
	if err != nil {
		return nil, err
	}

	result := []*BlockDevice{}
	for _, curr := range bds {
		if !curr.IsAvailable() {
			continue
		}

		result = append(result, curr)
	}

	avBlockDevices = result
	return result, nil
}

// ListBlockDevices Lists all block devices
// userDefined will be inserted in the resulting list reather the loaded ones
func ListBlockDevices(userDefined []*BlockDevice) ([]*BlockDevice, error) {
	return listBlockDevices(userDefined)
}

// Equals compares two BlockDevice instances
func (bd *BlockDevice) Equals(cmp *BlockDevice) bool {
	if cmp == nil {
		return false
	}

	return bd.Name == cmp.Name && bd.Model == cmp.Model && bd.MajorMinor == cmp.MajorMinor
}

func parseBlockDevicesDescriptor(data []byte) ([]*BlockDevice, error) {
	root := struct {
		BlockDevices []*BlockDevice `json:"blockdevices"`
	}{}

	err := json.Unmarshal(data, &root)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	for _, bd := range root.BlockDevices {
		bd.available = true

		for _, ch := range bd.Children {
			ch.Parent = bd
			// We ignore devices with any mount partition
			if ch.MountPoint != "" {
				bd.available = false
				break
			}
			// We ignore devices if any partition has a CLR_ISO label
			if strings.Contains(ch.Label, "CLR_ISO") {
				bd.available = false
				break
			}
		}

		// We ignore devices if the filesystem is squashfs
		if strings.Contains(bd.FsType, "squashfs") {
			bd.available = false
		}
	}

	return root.BlockDevices, nil
}

func getNextStrToken(dec *json.Decoder, name string) (string, error) {
	t, _ := dec.Token()
	if t == nil {
		return "", nil
	}

	str, valid := t.(string)
	if !valid {
		return "", errors.Errorf("\"%s\" token should have a string value", name)
	}

	return str, nil
}

func getNextByteToken(dec *json.Decoder, name string) (uint64, error) {
	var byteSize uint64
	var err error

	dec.UseNumber()
	token, _ := dec.Token()
	if token == nil {
		return 0, nil
	}

	switch t := token.(type) {
	case json.Number:
		// Is it an unsigned int value (lsblk >= 2.33)
		var n int64

		n, err = t.Int64()
		if err != nil {
			return 0, err
		}

		byteSize = uint64(n)

	case string:
		// Is it a string value (lsblk < 2.33)

		str, sValid := token.(string)
		if !sValid {
			return 0, errors.Errorf("\"%s\" token is neither an uint64 nor a string value", name)
		}

		byteSize, err = ParseVolumeSize(str)
		if err != nil {
			return 0, err
		}
	}

	return byteSize, nil
}

func getNextBoolToken(dec *json.Decoder, name string) (bool, error) {
	t, _ := dec.Token()
	if t == nil {
		return false, nil
	}

	// Is it a boolean value (lsblk >= 2.33)
	b, bValid := t.(bool)
	if bValid {
		return b, nil
	}

	// Is it a string value (lsblk < 2.33)
	str, sValid := t.(string)
	if !sValid {
		return false, errors.Errorf("\"%s\" token is neither a boolean nor a string value", name)
	}

	if str == "0" {
		return false, nil
	} else if str == "1" {
		return true, nil
	} else if str == "" {
		return false, nil
	}

	return false, errors.Errorf("Unknown ro value: %s", str)
}

// IsValidLabel returns empty string if label is valid
func IsValidLabel(label string, fstype string) string {
	if label == "" {
		return ""
	}

	max := MaxLabelLength(fstype)
	if len(label) > max {
		return utils.Locale.Get("Label too long, max is %d", max)
	}

	if !labelExp.MatchString(label) {
		return utils.Locale.Get("Invalid label characters")
	}

	return ""
}

// IsValidMount returns empty string if mount point is a valid directory for mounting
func IsValidMount(str string) string {
	if !mountExp.MatchString(str) {
		return "Invalid mount point"
	}

	return ""
}

// IsValidSize returns an empty string if
// -- size is suffixed with B, K, M, G, T, P
// -- size is greater than MinimumPartitionSize
// -- size is less than (or equal to) current size + free space
func (bd *BlockDevice) IsValidSize(str string, maxPartSize uint64) string {
	str = strings.ToLower(str)

	if !storageExp.MatchString(str) {
		return utils.Locale.Get("Invalid size, may only be suffixed by: B, K, M, G, T or P")
	}

	size, err := ParseVolumeHumanSize(str)
	if err != nil {
		return utils.Locale.Get("Invalid size")
	} else if size < MinimumPartitionSize {
		return utils.Locale.Get("Size too small")
	}

	if maxPartSize == 0 {
		return utils.Locale.Get("Unknown free space")
	} else if size > maxPartSize {
		return utils.Locale.Get("Size too large")
	}

	return ""
}

// ParseVolumeSize will parse a string formatted (1M, 10G, 2T) size and return its representation
// in bytes
func ParseVolumeSize(str string) (uint64, error) {
	var size uint64

	str = strings.ToLower(str)

	if !storageExp.MatchString(str) {
		return strconv.ParseUint(str, 0, 64)
	}

	unit := storageExp.ReplaceAllString(str, `$3`)
	fsize, err := strconv.ParseFloat(storageExp.ReplaceAllString(str, `$1`), 64)
	if err != nil {
		return 0, errors.Wrap(err)
	}

	switch unit {
	case "b":
		fsize = fsize * (1 << 0)
	case "k":
		fsize = fsize * (1 << 10)
	case "m":
		fsize = fsize * (1 << 20)
	case "g":
		fsize = fsize * (1 << 30)
	case "t":
		fsize = fsize * (1 << 40)
	case "p":
		fsize = fsize * (1 << 50)
	}

	size = uint64(math.Round(fsize))

	return size, nil
}

// ParseVolumeHumanSize will parse a string formatted (1M, 10G, 2T) size and
// return its representation in human bytes; MB, not MiB
func ParseVolumeHumanSize(str string) (uint64, error) {
	var size uint64

	str = strings.ToLower(str)

	if !storageExp.MatchString(str) {
		return strconv.ParseUint(str, 0, 64)
	}

	unit := storageExp.ReplaceAllString(str, `$3`)
	fsize, err := strconv.ParseFloat(storageExp.ReplaceAllString(str, `$1`), 64)
	if err != nil {
		return 0, errors.Wrap(err)
	}

	switch unit {
	case "b":
		fsize = fsize * (1.0)
	case "k":
		fsize = fsize * (1.0 * 1000.0)
	case "m":
		fsize = fsize * (1.0 * 1000.0 * 1000.0)
	case "g":
		fsize = fsize * (1.0 * 1000.0 * 1000.0 * 1000.0)
	case "t":
		fsize = fsize * (1.0 * 1000.0 * 1000.0 * 1000.0 * 1000.0)
	case "p":
		fsize = fsize * (1.0 * 1000.0 * 1000.0 * 1000.0 * 1000.0 * 1000.0)
	}

	size = uint64(math.Round(fsize))

	return size, nil
}

// UnmarshalJSON decodes a BlockDevice, targeted to integrate with json
// decoding framework
func (bd *BlockDevice) UnmarshalJSON(b []byte) error {

	dec := json.NewDecoder(bytes.NewReader(b))

	for {
		t, err := dec.Token()
		if err == io.EOF {
			break
		}

		str, valid := t.(string)
		if !valid {
			continue
		}

		switch str {
		case "name":
			var name string

			name, err = getNextStrToken(dec, "name")
			if err != nil {
				return err
			}

			bd.Name = name
		case "model":
			var model string

			model, err = getNextStrToken(dec, "model")
			if err != nil {
				return err
			}

			bd.Model = model
		case "maj:min":
			var majMin string

			majMin, err = getNextStrToken(dec, "maj:min")
			if err != nil {
				return err
			}

			bd.MajorMinor = majMin
		case "size":
			var size uint64

			size, err = getNextByteToken(dec, "size")
			if err != nil {
				return err
			}

			bd.Size = size
		case "pttype":
			var pttype string

			pttype, err = getNextStrToken(dec, "pttype")
			if err != nil {
				return err
			}

			bd.PtType = pttype
		case "fstype":
			var fstype string

			fstype, err = getNextStrToken(dec, "fstype")
			if err != nil {
				return err
			}

			bd.FsType = fstype
		case "uuid":
			var uuid string

			uuid, err = getNextStrToken(dec, "uuid")
			if err != nil {
				return err
			}

			bd.UUID = uuid
		case "serial":
			var serial string

			serial, err = getNextStrToken(dec, "serial")
			if err != nil {
				return err
			}

			bd.Serial = serial
		case "type":
			var tp string

			tp, err = getNextStrToken(dec, "type")
			if err != nil {
				return err
			}

			bd.Type, err = parseBlockDeviceType(tp)
			if err != nil {
				return err
			}
		case "state":
			var state string

			state, err = getNextStrToken(dec, "state")
			if err != nil {
				return err
			}

			bd.State, err = parseBlockDeviceState(state)
			if err != nil {
				return err
			}
		case "mountpoint":
			var mpoint string

			mpoint, err = getNextStrToken(dec, "mountpoint")
			if err != nil {
				return err
			}

			bd.MountPoint = mpoint
		case "label":
			var label string

			label, err = getNextStrToken(dec, "label")
			if err != nil {
				return err
			}

			bd.Label = label
		case "ro":
			bd.ReadOnly, err = getNextBoolToken(dec, "ro")
			if err != nil {
				return err
			}
		case "rm":
			bd.RemovableDevice, err = getNextBoolToken(dec, "rm")
			if err != nil {
				return err
			}
		case "children":
			bd.Children = []*BlockDevice{}
			err := dec.Decode(&bd.Children)
			if err != nil {
				return errors.Errorf("Invalid \"children\" token: %s", err)
			}
		}
	}

	return nil
}

// MarshalYAML is the yaml Marshaller implementation
func (bd *BlockDevice) MarshalYAML() (interface{}, error) {

	var bdm blockDeviceYAMLMarshal

	bdm.Name = bd.Name
	bdm.Model = bd.Model
	bdm.MajorMinor = bd.MajorMinor
	bdm.FsType = bd.FsType
	bdm.UUID = bd.UUID
	bdm.Serial = bd.Serial
	bdm.MountPoint = bd.MountPoint
	bdm.Label = bd.Label
	bdm.Size = strconv.FormatUint(bd.Size, 10)
	bdm.ReadOnly = strconv.FormatBool(bd.ReadOnly)
	bdm.RemovableDevice = strconv.FormatBool(bd.RemovableDevice)
	bdm.Type = bd.Type.String()
	bdm.State = bd.State.String()
	bdm.Children = bd.Children
	bdm.Options = bd.Options

	return bdm, nil
}

// UnmarshalYAML is the yaml Unmarshaller implementation
func (bd *BlockDevice) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var unmarshBlockDevice blockDeviceYAMLMarshal

	if err := unmarshal(&unmarshBlockDevice); err != nil {
		return err
	}

	// Copy the unmarshaled data
	bd.Name = unmarshBlockDevice.Name
	bd.Model = unmarshBlockDevice.Model
	bd.MajorMinor = unmarshBlockDevice.MajorMinor
	bd.FsType = unmarshBlockDevice.FsType
	bd.UUID = unmarshBlockDevice.UUID
	bd.Serial = unmarshBlockDevice.Serial
	bd.MountPoint = unmarshBlockDevice.MountPoint
	bd.Label = unmarshBlockDevice.Label
	bd.Children = unmarshBlockDevice.Children
	bd.Options = unmarshBlockDevice.Options
	// Convert String to Uint64
	if unmarshBlockDevice.Size != "" {
		uSize, err := ParseVolumeSize(unmarshBlockDevice.Size)
		if err != nil {
			return err
		}
		bd.Size = uSize
	}

	// Map the BlockDeviceType
	if unmarshBlockDevice.Type != "" {
		iType, err := parseBlockDeviceType(unmarshBlockDevice.Type)
		if err != nil {
			return errors.Errorf("Device: %s: %v", unmarshBlockDevice.Name, err)
		}
		if iType < 0 || iType > BlockDeviceTypeUnknown {
		}
		bd.Type = iType
		if iType != BlockDeviceTypeDisk {
			bd.MakePartition = true
			bd.FormatPartition = true
		}
	}

	// Map the BlockDeviceState
	if unmarshBlockDevice.State != "" {
		iState, err := parseBlockDeviceState(unmarshBlockDevice.State)
		if err != nil {
			return errors.Errorf("Device: %s: %v", unmarshBlockDevice.Name, err)
		}
		bd.State = iState
	}

	// Map the ReanOnly bool
	if unmarshBlockDevice.ReadOnly != "" {
		bReadOnly, err := strconv.ParseBool(unmarshBlockDevice.ReadOnly)
		if err != nil {
			return err
		}
		bd.ReadOnly = bReadOnly
	}

	// Map the RemovableDevice bool
	if unmarshBlockDevice.RemovableDevice != "" {
		bRemovableDevice, err := strconv.ParseBool(unmarshBlockDevice.RemovableDevice)
		if err != nil {
			return err
		}
		bd.RemovableDevice = bRemovableDevice
	}

	return nil
}

// SupportedFileSystems exposes the currently supported file system
func SupportedFileSystems() []string {
	res := []string{}

	for key := range bdOps {
		res = append(res, key)
	}

	sort.Strings(res)
	return res
}

// LargestFileSystemName returns the length of the largest supported file system name
func LargestFileSystemName() int {
	res := 0

	for key := range bdOps {
		fsl := len(key)
		if fsl > res {
			res = fsl
		}
	}

	return res
}

// MaxLabelLength returns the maximum length of a label for
// the given file system type
func MaxLabelLength(fstype string) int {
	var maxLen int

	switch fstype {
	case "ext2", "ext3", "ext4":
		maxLen = 16
	case "swap":
		maxLen = 15
	case "xfs":
		maxLen = 12
	case "btrfs":
		maxLen = 255
	case "vfat":
		maxLen = 11
	default:
		maxLen = 11
		log.Warning("Unknown file system type %s, defaulting to %d character label", fstype, maxLen)
	}

	return maxLen
}

// AddBootStandardPartition will add to disk a new standard Boot partition
func AddBootStandardPartition(disk *BlockDevice) uint64 {
	freePart := disk.findFree(bootSize)
	disk.AddFromFreePartition(freePart, &BlockDevice{
		Size:            bootSize,
		Type:            BlockDeviceTypePart,
		FsType:          "vfat",
		MountPoint:      "/boot",
		Label:           "boot",
		UserDefined:     true,
		MakePartition:   true,
		FormatPartition: true,
	})

	return bootSize
}

// AddSwapStandardPartition will add to disk a new standard Swap partition
func AddSwapStandardPartition(disk *BlockDevice) uint64 {
	freePart := disk.findFree(swapSize)
	disk.AddFromFreePartition(freePart, &BlockDevice{
		Size:            swapSize,
		Type:            BlockDeviceTypePart,
		FsType:          "swap",
		Label:           "swap",
		UserDefined:     true,
		MakePartition:   true,
		FormatPartition: true,
	})

	return swapSize
}

// AddRootStandardPartition will add to disk a new standard Root partition
func AddRootStandardPartition(disk *BlockDevice, rootSize uint64) {
	freePart := disk.findFree(rootSize)
	disk.AddFromFreePartition(freePart, &BlockDevice{
		Size:            rootSize,
		Type:            BlockDeviceTypePart,
		FsType:          "ext4",
		MountPoint:      "/",
		Label:           "root",
		UserDefined:     true,
		MakePartition:   true,
		FormatPartition: true,
	})
}

// NewStandardPartitions will add to disk a new set of partitions representing a
// default set of partitions required for an installation
func NewStandardPartitions(disk *BlockDevice) {
	disk.Children = nil
	newFreePart := &PartedPartition{
		Number:     0,
		Start:      0,
		End:        disk.Size,
		Size:       disk.Size,
		FileSystem: "free",
	}
	disk.PartTable = nil
	disk.PartTable = append(disk.PartTable, newFreePart)

	rootSize := uint64(disk.Size - bootSize - swapSize)

	freePart := disk.findFree(bootSize)
	disk.AddFromFreePartition(freePart, &BlockDevice{
		Size:            bootSize,
		Type:            BlockDeviceTypePart,
		FsType:          "vfat",
		MountPoint:      "/boot",
		Label:           "boot",
		UserDefined:     true,
		MakePartition:   true,
		FormatPartition: true,
	})

	freePart = disk.findFree(swapSize)
	disk.AddFromFreePartition(freePart, &BlockDevice{
		Size:            swapSize,
		Type:            BlockDeviceTypePart,
		FsType:          "swap",
		Label:           "swap",
		UserDefined:     true,
		MakePartition:   true,
		FormatPartition: true,
	})

	freePart = disk.findFree(rootSize)
	disk.AddFromFreePartition(freePart, &BlockDevice{
		Size:            rootSize,
		Type:            BlockDeviceTypePart,
		FsType:          "ext4",
		MountPoint:      "/",
		Label:           "root",
		UserDefined:     true,
		MakePartition:   true,
		FormatPartition: true,
	})
}

// PartProbe runs partprobe against the block device's file
func (bd *BlockDevice) PartProbe() error {
	args := []string{
		"partprobe",
		bd.GetDeviceFile(),
	}

	if err := cmd.RunAndLog(args...); err != nil {
		log.Warning("PartProbe has non-zero exit status: %s", err)
	}

	return nil
}

// DiskSize given a BlockDevice sum's up its size and children sizes
func (bd *BlockDevice) DiskSize() (uint64, error) {
	diskSize := bd.Size

	// otherwise, sum the children partitions to determine disk size
	var childSize uint64

	for _, ch := range bd.Children {
		if len(ch.Children) > 0 {
			size, err := ch.DiskSize()
			if err != nil {
				return 0, err
			}
			childSize += size
		} else {
			childSize += ch.Size
		}
	}

	if diskSize > 0 && childSize > diskSize {
		return 0, errors.Errorf("%s: Partition Sizes %d larger than Device Size: %d",
			bd.Name, childSize, diskSize)
	}

	// Return the Entire disk size if present
	if diskSize > 0 {
		return diskSize, nil
	}

	return childSize, nil
}

func (bd *BlockDevice) logDetails() {
	log.Debug(bd.Name, bd.FsType, bd.MountPoint, bd.Size, bd.Type)
}
