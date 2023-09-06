/*
 * Copyright (c) 2023. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package snapshot

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/nydus-snapshotter/config/daemonconfig"
	"github.com/containerd/nydus-snapshotter/pkg/layout"
	"github.com/containerd/nydus-snapshotter/pkg/rafs"
	"github.com/pkg/errors"
)

type ExtraOption struct {
	Source      string `json:"source"`
	Config      string `json:"config"`
	Snapshotdir string `json:"snapshotdir"`
	Version     string `json:"fs_version"`
}

func (o *snapshotter) remoteMountWithExtraOptions(ctx context.Context, s storage.Snapshot, id string, overlayOptions []string) ([]mount.Mount, error) {
	source, err := o.fs.BootstrapFile(id)
	if err != nil {
		return nil, err
	}

	instance := rafs.RafsGlobalCache.Get(id)
	daemon, err := o.fs.GetDaemonByID(instance.DaemonID)
	if err != nil {
		return nil, errors.Wrapf(err, "get daemon with ID %s", instance.DaemonID)
	}

	var c daemonconfig.DaemonConfig
	if daemon.IsSharedDaemon() {
		c, err = daemonconfig.NewDaemonConfig(daemon.States.FsDriver, daemon.ConfigFile(instance.SnapshotID))
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to load instance configuration %s",
				daemon.ConfigFile(instance.SnapshotID))
		}
	} else {
		c = daemon.Config
	}
	configContent, err := c.DumpString()
	if err != nil {
		return nil, errors.Wrapf(err, "remoteMounts: failed to marshal config")
	}

	// get version from bootstrap
	f, err := os.Open(source)
	if err != nil {
		return nil, errors.Wrapf(err, "remoteMounts: check bootstrap version: failed to open bootstrap")
	}
	defer f.Close()
	header := make([]byte, 4096)
	sz, err := f.Read(header)
	if err != nil {
		return nil, errors.Wrapf(err, "remoteMounts: check bootstrap version: failed to read bootstrap")
	}
	version, err := layout.DetectFsVersion(header[0:sz])
	if err != nil {
		return nil, errors.Wrapf(err, "remoteMounts: failed to detect filesystem version")
	}

	// when enable nydus-overlayfs, return unified mount slice for runc and kata
	extraOption := &ExtraOption{
		Source:      source,
		Config:      configContent,
		Snapshotdir: o.snapshotDir(s.ID),
		Version:     version,
	}
	no, err := json.Marshal(extraOption)
	if err != nil {
		return nil, errors.Wrapf(err, "remoteMounts: failed to marshal NydusOption")
	}
	// XXX: Log options without extraoptions as it might contain secrets.
	log.G(ctx).Debugf("fuse.nydus-overlayfs mount options %v", overlayOptions)
	// base64 to filter easily in `nydus-overlayfs`
	opt := fmt.Sprintf("extraoption=%s", base64.StdEncoding.EncodeToString(no))
	overlayOptions = append(overlayOptions, opt)

	return []mount.Mount{
		{
			Type:    "fuse.nydus-overlayfs",
			Source:  "overlay",
			Options: overlayOptions,
		},
	}, nil
}

// Consts and data structures for Kata Virtual Volume
const (
	minBlockSize = 1 << 9
	maxBlockSize = 1 << 19
)

const (
	KataVirtualVolumeOptionName          = "io.katacontainers.volume"
	KataVirtualVolumeDirectBlockType     = "direct_block"
	KataVirtualVolumeImageRawBlockType   = "image_raw_block"
	KataVirtualVolumeLayerRawBlockType   = "layer_raw_block"
	KataVirtualVolumeImageNydusBlockType = "image_nydus_block"
	KataVirtualVolumeLayerNydusBlockType = "layer_nydus_block"
	KataVirtualVolumeImageNydusFsType    = "image_nydus_fs"
	KataVirtualVolumeLayerNydusFsType    = "layer_nydus_fs"
	KataVirtualVolumeImageGuestPullType  = "image_guest_pull"
)

// DmVerityInfo contains configuration information for DmVerity device.
type DmVerityInfo struct {
	HashType  string `json:"hashtype"`
	Hash      string `json:"hash"`
	BlockNum  uint64 `json:"blocknum"`
	Blocksize uint64 `json:"blocksize"`
	Hashsize  uint64 `json:"hashsize"`
	Offset    uint64 `json:"offset"`
}

func (d *DmVerityInfo) IsValid() error {
	err := d.validateHashType()
	if err != nil {
		return err
	}

	if d.BlockNum == 0 || d.BlockNum > uint64(^uint32(0)) {
		return fmt.Errorf("Zero block count for DmVerity device %s", d.Hash)
	}

	if !isValidBlockSize(d.Blocksize) || !isValidBlockSize(d.Hashsize) {
		return fmt.Errorf("Unsupported verity block size: data_block_size = %d, hash_block_size = %d", d.Blocksize, d.Hashsize)
	}

	if d.Offset%d.Hashsize != 0 || d.Offset < d.Blocksize*d.BlockNum {
		return fmt.Errorf("Invalid hashvalue offset %d for DmVerity device %s", d.Offset, d.Hash)
	}

	return nil
}

func (d *DmVerityInfo) validateHashType() error {
	switch strings.ToLower(d.HashType) {
	case "sha256":
		return d.isValidHash(64, "sha256")
	case "sha1":
		return d.isValidHash(40, "sha1")
	default:
		return fmt.Errorf("Unsupported hash algorithm %s for DmVerity device %s", d.HashType, d.Hash)
	}
}

func (d *DmVerityInfo) isValidHash(expectedLen int, hashType string) error {
	_, err := hex.DecodeString(d.Hash)
	if len(d.Hash) != expectedLen || err != nil {
		return fmt.Errorf("Invalid hash value %s:%s for DmVerity device with %s", hashType, d.Hash, hashType)
	}
	return nil
}

func isValidBlockSize(blockSize uint64) bool {
	return minBlockSize <= blockSize && blockSize <= maxBlockSize
}

func ParseDmVerityInfo(option string) (*DmVerityInfo, error) {
	no := &DmVerityInfo{}
	if err := json.Unmarshal([]byte(option), no); err != nil {
		return nil, errors.Wrapf(err, "DmVerityInfo json unmarshal err")
	}
	if err := no.IsValid(); err != nil {
		return nil, fmt.Errorf("DmVerityInfo is not correct, %+v; error = %+v", no, err)
	}
	return no, nil
}

// DirectAssignedVolume contains meta information for a directly assigned volume.
type DirectAssignedVolume struct {
	Metadata map[string]string `json:"metadata"`
}

func (d *DirectAssignedVolume) IsValid() bool {
	return d.Metadata != nil
}

// ImagePullVolume contains meta information for pulling an image inside the guest.
type ImagePullVolume struct {
	Metadata map[string]string `json:"metadata"`
}

func (i *ImagePullVolume) IsValid() bool {
	return i.Metadata != nil
}

// NydusImageVolume contains Nydus image volume information.
type NydusImageVolume struct {
	Config      string `json:"config"`
	SnapshotDir string `json:"snapshot_dir"`
}

func (n *NydusImageVolume) IsValid() bool {
	return len(n.Config) > 0 || len(n.SnapshotDir) > 0
}

// KataVirtualVolume encapsulates information for extra mount options and direct volumes.
type KataVirtualVolume struct {
	VolumeType   string                `json:"volume_type"`
	Source       string                `json:"source,omitempty"`
	FSType       string                `json:"fs_type,omitempty"`
	Options      []string              `json:"options,omitempty"`
	DirectVolume *DirectAssignedVolume `json:"direct_volume,omitempty"`
	ImagePull    *ImagePullVolume      `json:"image_pull,omitempty"`
	NydusImage   *NydusImageVolume     `json:"nydus_image,omitempty"`
	DmVerity     *DmVerityInfo         `json:"dm_verity,omitempty"`
}

func (k *KataVirtualVolume) IsValid() bool {
	switch k.VolumeType {
	case KataVirtualVolumeDirectBlockType:
		if k.Source != "" && k.DirectVolume != nil && k.DirectVolume.IsValid() {
			return true
		}
	case KataVirtualVolumeImageRawBlockType, KataVirtualVolumeLayerRawBlockType:
		if k.Source != "" && (k.DmVerity == nil || k.DmVerity.IsValid() == nil) {
			return true
		}
	case KataVirtualVolumeImageNydusBlockType, KataVirtualVolumeLayerNydusBlockType, KataVirtualVolumeImageNydusFsType, KataVirtualVolumeLayerNydusFsType:
		if k.Source != "" && k.NydusImage != nil && k.NydusImage.IsValid() {
			return true
		}
	case KataVirtualVolumeImageGuestPullType:
		if k.Source != "" && k.ImagePull != nil && k.ImagePull.IsValid() {
			return true
		}
	}

	return false
}

func ParseKataVirtualVolume(option []byte) (*KataVirtualVolume, error) {
	no := &KataVirtualVolume{}
	if err := json.Unmarshal(option, no); err != nil {
		return nil, errors.Wrapf(err, "KataVirtualVolume json unmarshal err")
	}
	if !no.IsValid() {
		return nil, fmt.Errorf("KataVirtualVolume is not correct, %+v", no)
	}

	return no, nil
}

func ParseKataVirtualVolumeFromBase64(option string) (*KataVirtualVolume, error) {
	opt, err := base64.StdEncoding.DecodeString(option)
	if err != nil {
		return nil, errors.Wrap(err, "KataVirtualVolume base64 decoding err")
	}
	return ParseKataVirtualVolume(opt)
}

func EncodeKataVirtualVolumeToBase64(volume KataVirtualVolume) (string, error) {
	validKataVirtualVolumeJSON, err := json.Marshal(volume)
	if err != nil {
		return "", errors.Wrapf(err, "marshal KataVirtualVolume object")
	}
	option := base64.StdEncoding.EncodeToString(validKataVirtualVolumeJSON)
	return option, nil
}
