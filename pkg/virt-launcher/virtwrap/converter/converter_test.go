/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package converter

import (
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	"github.com/onsi/gomega/types"
	"go.uber.org/mock/gomock"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	k8smeta "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	kvapi "kubevirt.io/client-go/api"

	"kubevirt.io/kubevirt/pkg/config"
	"kubevirt.io/kubevirt/pkg/defaults"
	"kubevirt.io/kubevirt/pkg/downwardmetrics"
	"kubevirt.io/kubevirt/pkg/ephemeral-disk/fake"
	cmdv1 "kubevirt.io/kubevirt/pkg/handler-launcher-com/cmd/v1"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/os/disk"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	archconverter "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/arch"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/vcpu"
	sev "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/launchsecurity"
)

var (
	//go:embed testdata/domain_x86_64.xml.tmpl
	embedDomainTemplateX86_64 string
	//go:embed testdata/domain_ppc64le.xml.tmpl
	embedDomainTemplatePPC64le string
	//go:embed testdata/domain_arm64.xml.tmpl
	embedDomainTemplateARM64 string
	//go:embed testdata/domain_s390x.xml.tmpl
	embedDomainTemplateS390X string
	//go:embed testdata/domain_x86_64_root.xml.tmpl
	embedDomainTemplateRootBus string
)

const (
	blockPVCName = "pvc_block_test"
	amd64        = "amd64"
	arm64        = "arm64"
	ppc64le      = "ppc64le"
	s390x        = "s390x"
)

// MultiArchEntry returns a slice of Ginkgo TableEntry starting from one.
// It repeats the same TableEntry for every architecture.
// This is pretty useful when the same behavior is expected for every arch.
// **IMPORTANT**
// This requires the DescribeTable body func to have `arch string` as first
// parameter.
func MultiArchEntry(text string, args ...interface{}) []TableEntry {
	return []TableEntry{
		Entry(fmt.Sprintf("%s on %s", text, amd64), append([]interface{}{amd64}, args...)...),
		Entry(fmt.Sprintf("%s on %s", text, arm64), append([]interface{}{arm64}, args...)...),
		Entry(fmt.Sprintf("%s on %s", text, ppc64le), append([]interface{}{ppc64le}, args...)...),
		Entry(fmt.Sprintf("%s on %s", text, s390x), append([]interface{}{s390x}, args...)...),
	}
}

func memBalloonWithModelAndPeriod(model string, period int) string {
	const argMemBalloonFmt = `<memballoon model="%s" freePageReporting="on">%s</memballoon>`
	if model == "none" {
		return `<memballoon model="none"></memballoon>`
	}

	if period == 0 {
		return fmt.Sprintf(argMemBalloonFmt, model, "")
	}

	return fmt.Sprintf(argMemBalloonFmt, model, fmt.Sprintf(`
      <stats period="%d"></stats>
    `, period))

}

var _ = Describe("getOptimalBlockIO", func() {

	It("Should detect disk block sizes for a file DiskSource", func() {
		disk := &api.Disk{
			Source: api.DiskSource{
				File: "/",
			},
		}
		blockIO, err := getOptimalBlockIO(disk)
		Expect(err).ToNot(HaveOccurred())
		Expect(blockIO.LogicalBlockSize).To(Equal(blockIO.PhysicalBlockSize))
		// The default for most filesystems nowadays is 4096 but it can be changed.
		// As such, relying on a specific value is flakey unless
		// we create a disk image and filesystem just for this test.
		// For now, as long as we have a value, the exact value doesn't matter.
		Expect(blockIO.LogicalBlockSize).ToNot(BeZero())
	})

	It("Should fail for non-file or non-block devices", func() {
		disk := &api.Disk{
			Source: api.DiskSource{},
		}
		_, err := getOptimalBlockIO(disk)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Converter", func() {

	TestSmbios := &cmdv1.SMBios{}
	EphemeralDiskImageCreator := &fake.MockEphemeralDiskImageCreator{BaseDir: "/var/run/libvirt/kubevirt-ephemeral-disk/"}

	Context("with watchdog", func() {
		DescribeTable("should successfully convert watchdog for supported architectures",
			func(arch string, input *v1.Watchdog, expected *api.Watchdog) {
				converter := archconverter.NewConverter(arch)
				newWatchdog := &api.Watchdog{}
				err := converter.ConvertWatchdog(input, newWatchdog)

				Expect(err).ToNot(HaveOccurred())
				Expect(newWatchdog).To(Equal(expected))
			},

			Entry("amd64 with I6300ESB",
				"amd64",
				&v1.Watchdog{
					Name: "mywatchdog",
					WatchdogDevice: v1.WatchdogDevice{
						I6300ESB: &v1.I6300ESBWatchdog{
							Action: v1.WatchdogActionPoweroff,
						},
					},
				},
				&api.Watchdog{
					Alias:  api.NewUserDefinedAlias("mywatchdog"),
					Model:  "i6300esb",
					Action: "poweroff",
				},
			),

			Entry("s390x with Diag288",
				"s390x",
				&v1.Watchdog{
					Name: "diagwatchdog",
					WatchdogDevice: v1.WatchdogDevice{
						Diag288: &v1.Diag288Watchdog{
							Action: v1.WatchdogActionReset,
						},
					},
				},
				&api.Watchdog{
					Alias:  api.NewUserDefinedAlias("diagwatchdog"),
					Model:  "diag288",
					Action: "reset",
				},
			),
		)
		DescribeTable("should fail to convert watchdog for unsupported or invalid architectures",
			func(arch string, input *v1.Watchdog, expectedErrMsg string) {
				converter := archconverter.NewConverter(arch)
				newWatchdog := &api.Watchdog{}
				err := converter.ConvertWatchdog(input, newWatchdog)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(expectedErrMsg))
			},

			Entry("arm64 not supported",
				"arm64",
				&v1.Watchdog{Name: "unsupportedwatchdog"},
				"not supported on architecture",
			),

			Entry("ppc64le not supported",
				"ppc64le",
				&v1.Watchdog{Name: "unsupportedwatchdog"},
				"not supported on architecture",
			),

			Entry("amd64 with no watchdog type",
				"amd64",
				&v1.Watchdog{Name: "emptywatchdog"},
				"can't be mapped",
			),

			Entry("s390x with nil Diag288",
				"s390x",
				&v1.Watchdog{Name: "diagwatchdog"},
				"can't be mapped",
			),
		)

	})

	Context("with timezone", func() {
		It("Should set timezone attribute", func() {
			timezone := v1.ClockOffsetTimezone("America/New_York")
			clock := &v1.Clock{
				ClockOffset: v1.ClockOffset{
					Timezone: &timezone,
				},
				Timer: &v1.Timer{},
			}

			var convertClock api.Clock
			Convert_v1_Clock_To_api_Clock(clock, &convertClock)
			data, err := xml.MarshalIndent(convertClock, "", "  ")
			Expect(err).ToNot(HaveOccurred())

			expectedClock := `<Clock offset="timezone" timezone="America/New_York"></Clock>`
			Expect(string(data)).To(Equal(expectedClock))
		})
	})

	Context("with v1.Disk", func() {
		DescribeTable("Should define disk capacity as the minimum of capacity and request", func(arch string, requests, capacity, expected int64) {
			context := &ConverterContext{Architecture: archconverter.NewConverter(arch)}
			v1Disk := v1.Disk{
				Name: "myvolume",
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{Bus: v1.VirtIO},
				},
			}
			apiDisk := api.Disk{}
			devicePerBus := map[string]deviceNamer{}
			numQueues := uint(2)
			volumeStatusMap := make(map[string]v1.VolumeStatus)
			volumeStatusMap["myvolume"] = v1.VolumeStatus{
				PersistentVolumeClaimInfo: &v1.PersistentVolumeClaimInfo{
					Capacity: k8sv1.ResourceList{
						k8sv1.ResourceStorage: *resource.NewQuantity(capacity, resource.DecimalSI),
					},
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceStorage: *resource.NewQuantity(requests, resource.DecimalSI),
					},
				},
			}
			Convert_v1_Disk_To_api_Disk(context, &v1Disk, &apiDisk, devicePerBus, &numQueues, volumeStatusMap)
			Expect(apiDisk.Capacity).ToNot(BeNil())
			Expect(*apiDisk.Capacity).To(Equal(expected))
		},
			MultiArchEntry("Higher request than capacity", int64(9999), int64(1111), int64(1111)),
			MultiArchEntry("Lower request than capacity", int64(1111), int64(9999), int64(1111)),
		)

		DescribeTable("Should assign scsi controller to", func(diskDevice v1.DiskDevice) {
			context := &ConverterContext{}
			v1Disk := v1.Disk{
				Name:       "myvolume",
				DiskDevice: diskDevice,
			}
			apiDisk := api.Disk{}
			devicePerBus := map[string]deviceNamer{}
			numQueues := uint(2)
			volumeStatusMap := make(map[string]v1.VolumeStatus)
			volumeStatusMap["myvolume"] = v1.VolumeStatus{}
			Convert_v1_Disk_To_api_Disk(context, &v1Disk, &apiDisk, devicePerBus, &numQueues, volumeStatusMap)
			Expect(apiDisk.Address).ToNot(BeNil())
			Expect(apiDisk.Address.Bus).To(Equal("0"))
			Expect(apiDisk.Address.Controller).To(Equal("0"))
			Expect(apiDisk.Address.Type).To(Equal("drive"))
			Expect(apiDisk.Address.Unit).To(Equal("0"))
		},
			Entry("LUN-type disk", v1.DiskDevice{
				LUN: &v1.LunTarget{Bus: "scsi"},
			}),
			Entry("Disk-type disk", v1.DiskDevice{
				Disk: &v1.DiskTarget{Bus: "scsi"},
			}),
		)

		DescribeTable("Should add boot order when provided", func(arch, expectedModel string) {
			order := uint(1)
			kubevirtDisk := &v1.Disk{
				Name:      "mydisk",
				BootOrder: &order,
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{
						Bus: v1.VirtIO,
					},
				},
			}
			convertedDisk := fmt.Sprintf(`<Disk device="disk" type="" model="%s">
  <source></source>
  <target bus="virtio" dev="vda"></target>
  <driver name="qemu" type="" discard="unmap"></driver>
  <alias name="ua-mydisk"></alias>
  <boot order="1"></boot>
</Disk>`, expectedModel)
			xml := diskToDiskXML(arch, kubevirtDisk)
			Expect(xml).To(Equal(convertedDisk))
		},
			Entry("on amd64", amd64, "virtio-non-transitional"),
			Entry("on arm64", arm64, "virtio-non-transitional"),
			Entry("on ppc64le", ppc64le, "virtio-non-transitional"),
			Entry("on s390x", s390x, "virtio"),
		)

		DescribeTable("should set disk I/O mode if requested", func(arch string) {
			v1Disk := &v1.Disk{
				IO: "native",
			}
			xml := diskToDiskXML(arch, v1Disk)
			expectedXML := `<Disk device="" type="">
  <source></source>
  <target></target>
  <driver io="native" name="qemu" type=""></driver>
  <alias name="ua-"></alias>
</Disk>`
			Expect(xml).To(Equal(expectedXML))
		},
			MultiArchEntry(""),
		)

		DescribeTable("should not set disk I/O mode if not requested", func(arch string) {
			v1Disk := &v1.Disk{}
			xml := diskToDiskXML(arch, v1Disk)
			expectedXML := `<Disk device="" type="">
  <source></source>
  <target></target>
  <driver name="qemu" type=""></driver>
  <alias name="ua-"></alias>
</Disk>`
			Expect(xml).To(Equal(expectedXML))
		},
			MultiArchEntry(""),
		)

		DescribeTable("Should omit boot order when not provided", func(arch, expectedModel string) {
			kubevirtDisk := &v1.Disk{
				Name: "mydisk",
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{
						Bus: v1.VirtIO,
					},
				},
			}
			var convertedDisk = fmt.Sprintf(`<Disk device="disk" type="" model="%s">
  <source></source>
  <target bus="virtio" dev="vda"></target>
  <driver name="qemu" type="" discard="unmap"></driver>
  <alias name="ua-mydisk"></alias>
</Disk>`, expectedModel)
			xml := diskToDiskXML(arch, kubevirtDisk)
			Expect(xml).To(Equal(convertedDisk))
		},
			Entry("on amd64", amd64, "virtio-non-transitional"),
			Entry("on arm64", arm64, "virtio-non-transitional"),
			Entry("on ppc64le", ppc64le, "virtio-non-transitional"),
			Entry("on s390x", s390x, "virtio"),
		)

		It("Should add blockio fields when custom sizes are provided", func() {
			kubevirtDisk := &v1.Disk{
				BlockSize: &v1.BlockSize{
					Custom: &v1.CustomBlockSize{
						Logical:  1234,
						Physical: 1234,
					},
				},
			}
			expectedXML := `<Disk device="" type="">
  <source></source>
  <target></target>
  <blockio logical_block_size="1234" physical_block_size="1234"></blockio>
</Disk>`
			libvirtDisk := &api.Disk{}
			Expect(Convert_v1_BlockSize_To_api_BlockIO(kubevirtDisk, libvirtDisk)).To(Succeed())
			data, err := xml.MarshalIndent(libvirtDisk, "", "  ")
			Expect(err).ToNot(HaveOccurred())
			xml := string(data)
			Expect(xml).To(Equal(expectedXML))
		})
		DescribeTable("should set sharable and the cache if requested", func(arch, expectedModel string) {
			v1Disk := &v1.Disk{
				Name: "mydisk",
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{
						Bus: v1.VirtIO,
					},
				},
				Shareable: pointer.P(true),
			}
			var expectedXML = fmt.Sprintf(`<Disk device="disk" type="" model="%s">
  <source></source>
  <target bus="virtio" dev="vda"></target>
  <driver cache="none" name="qemu" type="" discard="unmap"></driver>
  <alias name="ua-mydisk"></alias>
  <shareable></shareable>
</Disk>`, expectedModel)
			xml := diskToDiskXML(arch, v1Disk)
			Expect(xml).To(Equal(expectedXML))
		},
			Entry("on amd64", amd64, "virtio-non-transitional"),
			Entry("on arm64", arm64, "virtio-non-transitional"),
			Entry("on ppc64le", ppc64le, "virtio-non-transitional"),
			Entry("on s390x", s390x, "virtio"),
		)
	})

	Context("with v1.VirtualMachineInstance", func() {

		var vmi *v1.VirtualMachineInstance
		domainType := "kvm"
		if _, err := os.Stat("/dev/kvm"); errors.Is(err, os.ErrNotExist) {
			domainType = "qemu"
		}

		BeforeEach(func() {

			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "mynamespace",
				},
			}
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Clock = &v1.Clock{
				ClockOffset: v1.ClockOffset{
					UTC: &v1.ClockOffsetUTC{},
				},
				Timer: &v1.Timer{
					HPET: &v1.HPETTimer{
						Enabled:    pointer.P(false),
						TickPolicy: v1.HPETTickPolicyDelay,
					},
					KVM: &v1.KVMTimer{
						Enabled: pointer.P(true),
					},
					PIT: &v1.PITTimer{
						Enabled:    pointer.P(false),
						TickPolicy: v1.PITTickPolicyDiscard,
					},
					RTC: &v1.RTCTimer{
						Enabled:    pointer.P(true),
						TickPolicy: v1.RTCTickPolicyCatchup,
						Track:      v1.TrackGuest,
					},
					Hyperv: &v1.HypervTimer{
						Enabled: pointer.P(true),
					},
				},
			}
			vmi.Spec.Domain.Features = &v1.Features{
				APIC:       &v1.FeatureAPIC{},
				SMM:        &v1.FeatureState{},
				KVM:        &v1.FeatureKVM{Hidden: true},
				Pvspinlock: &v1.FeatureState{Enabled: pointer.P(false)},
				Hyperv: &v1.FeatureHyperv{
					Relaxed:         &v1.FeatureState{Enabled: pointer.P(false)},
					VAPIC:           &v1.FeatureState{Enabled: pointer.P(true)},
					Spinlocks:       &v1.FeatureSpinlocks{Enabled: pointer.P(true)},
					VPIndex:         &v1.FeatureState{Enabled: pointer.P(true)},
					Runtime:         &v1.FeatureState{Enabled: pointer.P(false)},
					SyNIC:           &v1.FeatureState{Enabled: pointer.P(true)},
					SyNICTimer:      &v1.SyNICTimer{Enabled: pointer.P(true), Direct: &v1.FeatureState{Enabled: pointer.P(true)}},
					Reset:           &v1.FeatureState{Enabled: pointer.P(true)},
					VendorID:        &v1.FeatureVendorID{Enabled: pointer.P(false), VendorID: "myvendor"},
					Frequencies:     &v1.FeatureState{Enabled: pointer.P(false)},
					Reenlightenment: &v1.FeatureState{Enabled: pointer.P(false)},
					TLBFlush:        &v1.FeatureState{Enabled: pointer.P(true)},
					IPI:             &v1.FeatureState{Enabled: pointer.P(true)},
					EVMCS:           &v1.FeatureState{Enabled: pointer.P(false)},
				},
			}
			vmi.Spec.Domain.Resources.Limits = make(k8sv1.ResourceList)
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{k8sv1.ResourceMemory: resource.MustParse("8192Ki")}
			vmi.Spec.Domain.Devices.DisableHotplug = true
			vmi.Spec.Domain.Devices.Inputs = []v1.Input{
				{
					Bus:  v1.VirtIO,
					Type: "tablet",
					Name: "tablet0",
				},
			}
			vmi.Spec.Domain.Devices.Disks = []v1.Disk{
				{
					Name: "myvolume",
					DiskDevice: v1.DiskDevice{
						Disk: &v1.DiskTarget{
							Bus: v1.VirtIO,
						},
					},
					DedicatedIOThread: pointer.P(true),
				},
				{
					Name: "nocloud",
					DiskDevice: v1.DiskDevice{
						Disk: &v1.DiskTarget{
							Bus: v1.VirtIO,
						},
					},
					DedicatedIOThread: pointer.P(true),
				},
				{
					Name: "cdrom_tray_unspecified",
					DiskDevice: v1.DiskDevice{
						CDRom: &v1.CDRomTarget{
							ReadOnly: pointer.P(false),
						},
					},
					DedicatedIOThread: pointer.P(false),
				},
				{
					Name: "cdrom_tray_open",
					DiskDevice: v1.DiskDevice{
						CDRom: &v1.CDRomTarget{
							Tray: v1.TrayStateOpen,
						},
					},
				},
				{
					Name: "should_default_to_disk",
				},
				{
					Name:  "ephemeral_pvc",
					Cache: "none",
				},
				{
					Name:   "secret_test",
					Serial: "D23YZ9W6WA5DJ487",
				},
				{
					Name:   "configmap_test",
					Serial: "CVLY623300HK240D",
				},
				{
					Name:  blockPVCName,
					Cache: "writethrough",
				},
				{
					Name:  "dv_block_test",
					Cache: "writethrough",
				},
				{
					Name: "serviceaccount_test",
				},
				{
					Name: "sysprep",
					DiskDevice: v1.DiskDevice{
						CDRom: &v1.CDRomTarget{
							ReadOnly: pointer.P(false),
						},
					},
					DedicatedIOThread: pointer.P(false),
				},
				{
					Name: "sysprep_secret",
					DiskDevice: v1.DiskDevice{
						CDRom: &v1.CDRomTarget{
							ReadOnly: pointer.P(false),
						},
					},
					DedicatedIOThread: pointer.P(false),
				},
			}
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "myvolume",
					VolumeSource: v1.VolumeSource{
						HostDisk: &v1.HostDisk{
							Path:     "/var/run/kubevirt-private/vmi-disks/myvolume/disk.img",
							Type:     v1.HostDiskExistsOrCreate,
							Capacity: resource.MustParse("1Gi"),
						},
					},
				},
				{
					Name: "nocloud",
					VolumeSource: v1.VolumeSource{
						CloudInitNoCloud: &v1.CloudInitNoCloudSource{
							UserDataBase64:    "1234",
							NetworkDataBase64: "1234",
						},
					},
				},
				{
					Name: "cdrom_tray_unspecified",
					VolumeSource: v1.VolumeSource{
						CloudInitNoCloud: &v1.CloudInitNoCloudSource{
							UserDataBase64:    "1234",
							NetworkDataBase64: "1234",
						},
					},
				},
				{
					Name: "cdrom_tray_open",
					VolumeSource: v1.VolumeSource{
						HostDisk: &v1.HostDisk{
							Path:     "/var/run/kubevirt-private/vmi-disks/volume1/disk.img",
							Type:     v1.HostDiskExistsOrCreate,
							Capacity: resource.MustParse("1Gi"),
						},
					},
				},
				{
					Name: "should_default_to_disk",
					VolumeSource: v1.VolumeSource{
						HostDisk: &v1.HostDisk{
							Path:     "/var/run/kubevirt-private/vmi-disks/volume4/disk.img",
							Type:     v1.HostDiskExistsOrCreate,
							Capacity: resource.MustParse("1Gi"),
						},
					},
				},
				{
					Name: "ephemeral_pvc",
					VolumeSource: v1.VolumeSource{
						Ephemeral: &v1.EphemeralVolumeSource{
							PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
								ClaimName: "testclaim",
							},
						},
					},
				},
				{
					Name: "secret_test",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: "testsecret",
						},
					},
				},
				{
					Name: "configmap_test",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{
							LocalObjectReference: k8sv1.LocalObjectReference{
								Name: "testconfig",
							},
						},
					},
				},
				{
					Name: blockPVCName,
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testblock",
						}},
					},
				},
				{
					Name: "dv_block_test",
					VolumeSource: v1.VolumeSource{
						DataVolume: &v1.DataVolumeSource{
							Name: "dv_block_test",
						},
					},
				},
				{
					Name: "serviceaccount_test",
					VolumeSource: v1.VolumeSource{
						ServiceAccount: &v1.ServiceAccountVolumeSource{
							ServiceAccountName: "testaccount",
						},
					},
				},
				{
					Name: "sysprep",
					VolumeSource: v1.VolumeSource{
						Sysprep: &v1.SysprepSource{
							ConfigMap: &k8sv1.LocalObjectReference{
								Name: "testconfig",
							},
						},
					},
				},
				{
					Name: "sysprep_secret",
					VolumeSource: v1.VolumeSource{
						Sysprep: &v1.SysprepSource{
							Secret: &k8sv1.LocalObjectReference{
								Name: "testsecret",
							},
						},
					},
				},
			}

			vmi.Spec.Networks = []v1.Network{*v1.DefaultPodNetwork()}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{*v1.DefaultBridgeNetworkInterface()}

			vmi.Spec.Domain.Firmware = &v1.Firmware{
				UUID:   "e4686d2c-6e8d-4335-b8fd-81bee22f4814",
				Serial: "e4686d2c-6e8d-4335-b8fd-81bee22f4815",
			}

			vmi.Spec.TerminationGracePeriodSeconds = pointer.P(int64(5))

			vmi.ObjectMeta.UID = "f4686d2c-6e8d-4335-b8fd-81bee22f4814"
		})

		var convertedDomain = strings.TrimSpace(fmt.Sprintf(embedDomainTemplateX86_64, domainType, "%s"))
		var convertedDomainWith5Period = fmt.Sprintf(convertedDomain, memBalloonWithModelAndPeriod("virtio-non-transitional", 5))
		var convertedDomainWith0Period = fmt.Sprintf(convertedDomain, memBalloonWithModelAndPeriod("virtio-non-transitional", 0))
		var convertedDomainWithFalseAutoattach = fmt.Sprintf(convertedDomain, memBalloonWithModelAndPeriod("none", 0))

		convertedDomain = fmt.Sprintf(convertedDomain, memBalloonWithModelAndPeriod("virtio-non-transitional", 10))

		var convertedDomainppc64le = strings.TrimSpace(fmt.Sprintf(embedDomainTemplatePPC64le, domainType, "%s"))
		var convertedDomainppc64leWith5Period = fmt.Sprintf(convertedDomainppc64le, memBalloonWithModelAndPeriod("virtio-non-transitional", 5))
		var convertedDomainppc64leWith0Period = fmt.Sprintf(convertedDomainppc64le, memBalloonWithModelAndPeriod("virtio-non-transitional", 0))
		var convertedDomainppc64leWithFalseAutoattach = fmt.Sprintf(convertedDomainppc64le, memBalloonWithModelAndPeriod("none", 0))

		convertedDomainppc64le = fmt.Sprintf(convertedDomainppc64le, memBalloonWithModelAndPeriod("virtio-non-transitional", 10))

		var convertedDomainarm64 = strings.TrimSpace(fmt.Sprintf(embedDomainTemplateARM64, domainType, "%s"))
		var convertedDomainarm64With5Period = fmt.Sprintf(convertedDomainarm64, memBalloonWithModelAndPeriod("virtio-non-transitional", 5))
		var convertedDomainarm64With0Period = fmt.Sprintf(convertedDomainarm64, memBalloonWithModelAndPeriod("virtio-non-transitional", 0))
		var convertedDomainarm64WithFalseAutoattach = fmt.Sprintf(convertedDomainarm64, memBalloonWithModelAndPeriod("none", 0))

		convertedDomainarm64 = fmt.Sprintf(convertedDomainarm64, memBalloonWithModelAndPeriod("virtio-non-transitional", 10))

		var convertedDomains390x = strings.TrimSpace(fmt.Sprintf(embedDomainTemplateS390X, domainType, "%s"))
		var convertedDomains390xWith5Period = fmt.Sprintf(convertedDomains390x, memBalloonWithModelAndPeriod("virtio", 5))
		var convertedDomains390xWith0Period = fmt.Sprintf(convertedDomains390x, memBalloonWithModelAndPeriod("virtio", 0))
		var convertedDomains390xWithFalseAutoattach = fmt.Sprintf(convertedDomains390x, memBalloonWithModelAndPeriod("none", 0))

		convertedDomains390x = fmt.Sprintf(convertedDomains390x, memBalloonWithModelAndPeriod("virtio", 10))

		var convertedDomainWithDevicesOnRootBus = strings.TrimSpace(fmt.Sprintf(embedDomainTemplateRootBus, domainType))

		var c *ConverterContext

		isBlockPVCMap := make(map[string]bool)
		isBlockPVCMap[blockPVCName] = true
		isBlockDVMap := make(map[string]bool)
		isBlockDVMap["dv_block_test"] = true

		BeforeEach(func() {
			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				VirtualMachine: vmi,
				Secrets: map[string]*k8sv1.Secret{
					"mysecret": {
						Data: map[string][]byte{
							"node.session.auth.username": []byte("admin"),
						},
					},
				},
				AllowEmulation:                  true,
				IsBlockPVC:                      isBlockPVCMap,
				IsBlockDV:                       isBlockDVMap,
				SMBios:                          TestSmbios,
				MemBalloonStatsPeriod:           10,
				EphemeraldiskCreator:            EphemeralDiskImageCreator,
				FreePageReporting:               true,
				SerialConsoleLog:                true,
				DomainAttachmentByInterfaceName: map[string]string{"default": string(v1.Tap)},
			}
		})

		DescribeTable("should use virtio-transitional models if requested", func(arch string) {
			c.Architecture = archconverter.NewConverter(arch)
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			vmi.Spec.Domain.Devices.DisableHotplug = false
			c.UseVirtioTransitional = true
			vmi.Spec.Domain.Devices.UseVirtioTransitional = &c.UseVirtioTransitional
			dom := vmiToDomain(vmi, c)
			testutils.ExpectVirtioTransitionalOnly(&dom.Spec)
		},
			Entry("on amd64 with success", amd64),
			Entry("on arm64 with success", arm64),
			Entry("on ppc64le with success", ppc64le),
			//TODO add s390x entry with custom check of model used (disks/interfaces/controllers/devices will use different models)
		)

		It("should handle float memory", func() {
			vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceMemory] = resource.MustParse("2222222200m")
			xml := vmiToDomainXML(vmi, c)
			Expect(strings.Contains(xml, `<memory unit="b">2222222</memory>`)).To(BeTrue(), xml)
		})

		It("should use panic devices if requested", func() {
			vmi.Spec.Domain.Devices.PanicDevices = []v1.PanicDevice{{Model: pointer.P(v1.Hyperv)}}
			xml := vmiToDomainXML(vmi, c)
			Expect(xml).To(ContainSubstring(`<panic model="hyperv"></panic>`))
		})

		DescribeTable("should be converted to a libvirt Domain with vmi defaults set", func(arch string, domain string) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			c.Architecture = archconverter.NewConverter(arch)
			vmiArchMutate(arch, vmi, c)
			Expect(vmiToDomainXML(vmi, c)).To(Equal(domain))
		},
			Entry("for amd64", amd64, convertedDomain),
			Entry("for ppc64le", ppc64le, convertedDomainppc64le),
			Entry("for arm64", arm64, convertedDomainarm64),
			Entry("for s390x", s390x, convertedDomains390x),
		)

		DescribeTable("should be converted to a libvirt Domain", func(arch string, domain string, period uint) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			c.Architecture = archconverter.NewConverter(arch)
			vmiArchMutate(arch, vmi, c)
			c.MemBalloonStatsPeriod = period
			Expect(vmiToDomainXML(vmi, c)).To(Equal(domain))
		},
			Entry("when context define 5 period on memballoon device for amd64", amd64, convertedDomainWith5Period, uint(5)),
			Entry("when context define 5 period on memballoon device for ppc64le", ppc64le, convertedDomainppc64leWith5Period, uint(5)),
			Entry("when context define 5 period on memballoon device for arm64", arm64, convertedDomainarm64With5Period, uint(5)),
			Entry("when context define 5 period on memballoon device for s390x", s390x, convertedDomains390xWith5Period, uint(5)),
			Entry("when context define 0 period on memballoon device for amd64 ", amd64, convertedDomainWith0Period, uint(0)),
			Entry("when context define 0 period on memballoon device for ppc64le", ppc64le, convertedDomainppc64leWith0Period, uint(0)),
			Entry("when context define 0 period on memballoon device for arm64", arm64, convertedDomainarm64With0Period, uint(0)),
			Entry("when context define 0 period on memballoon device for s390x", s390x, convertedDomains390xWith0Period, uint(0)),
		)

		DescribeTable("should be converted to a libvirt Domain", func(arch string, domain string) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			vmi.Spec.Domain.Devices.AutoattachMemBalloon = pointer.P(false)
			c.Architecture = archconverter.NewConverter(arch)
			vmiArchMutate(arch, vmi, c)
			Expect(vmiToDomainXML(vmi, c)).To(Equal(domain))
		},
			Entry("when Autoattach memballoon device is false for amd64", amd64, convertedDomainWithFalseAutoattach),
			Entry("when Autoattach memballoon device is false for ppc64le", ppc64le, convertedDomainppc64leWithFalseAutoattach),
			Entry("when Autoattach memballoon device is false for arm64", arm64, convertedDomainarm64WithFalseAutoattach),
			Entry("when Autoattach memballoon device is false for s390x", s390x, convertedDomains390xWithFalseAutoattach),
		)

		It("should use kvm if present", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			Expect(vmiToDomainXMLToDomainSpec(vmi, c).Type).To(Equal(domainType))
		})

		Context("when all addresses should be placed at the root complex", func() {
			It("should be converted to a libvirt Domain with vmi defaults set", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
				c.Architecture = archconverter.NewConverter(amd64)
				vmiArchMutate(amd64, vmi, c)
				spec := vmiToDomain(vmi, c).Spec.DeepCopy()
				Expect(PlacePCIDevicesOnRootComplex(spec)).To(Succeed())
				data, err := xml.MarshalIndent(spec, "", "  ")
				Expect(err).ToNot(HaveOccurred())
				Expect(string(data)).To(Equal(convertedDomainWithDevicesOnRootBus))
			})
		})

		Context("when CPU spec defined", func() {
			It("should convert CPU cores, model and features", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Cores:   3,
					Sockets: 2,
					Threads: 2,
					Model:   "Conroe",
					Features: []v1.CPUFeature{
						{
							Name:   "lahf_lm",
							Policy: "require",
						},
						{
							Name:   "mmx",
							Policy: "disable",
						},
					},
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(3)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(2)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(2)), "Expect threads")
				Expect(domainSpec.CPU.Mode).To(Equal("custom"), "Expect cpu mode")
				Expect(domainSpec.CPU.Model).To(Equal("Conroe"), "Expect cpu model")
				Expect(domainSpec.CPU.Features[0].Name).To(Equal("lahf_lm"), "Expect cpu feature name")
				Expect(domainSpec.CPU.Features[0].Policy).To(Equal("require"), "Expect cpu feature policy")
				Expect(domainSpec.CPU.Features[1].Name).To(Equal("mmx"), "Expect cpu feature name")
				Expect(domainSpec.CPU.Features[1].Policy).To(Equal("disable"), "Expect cpu feature policy")
				Expect(domainSpec.VCPU.Placement).To(Equal("static"), "Expect vcpu placement")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(12)), "Expect vcpus")
			})

			It("should convert CPU cores", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Cores: 3,
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(3)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(1)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			It("should convert CPU sockets", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Sockets: 3,
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(1)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(3)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			It("should convert CPU threads", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Threads: 3,
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(1)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(1)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(3)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			It("should convert CPU requests to sockets", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = nil
				vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("2200m")
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(1)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(3)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			It("should convert CPU limits to sockets", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = nil
				vmi.Spec.Domain.Resources.Limits[k8sv1.ResourceCPU] = resource.MustParse("2.3")
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(1)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(3)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			It("should prefer CPU spec instead of CPU requests", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Sockets: 3,
				}
				vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("400m")
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(1)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(3)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			It("should prefer CPU spec instead of CPU limits", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Sockets: 3,
				}
				vmi.Spec.Domain.Resources.Limits[k8sv1.ResourceCPU] = resource.MustParse("400m")
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(1)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(3)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(3)), "Expect vcpus")
			})

			DescribeTable("should define hotplugable default topology", func(arch string) {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Cores:      2,
					MaxSockets: 3,
					Sockets:    2,
				}
				c.Architecture = archconverter.NewConverter(arch)
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(2)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(3)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(6)), "Expect vcpus")
				Expect(domainSpec.VCPUs).ToNot(BeNil(), "Expecting topology for hotplug")
				Expect(domainSpec.VCPUs.VCPU).To(HaveLen(6), "Expecting topology for hotplug")
				Expect(domainSpec.VCPUs.VCPU[0].Hotpluggable).To(Equal("no"), "Expecting the 1st vcpu to be stable")
				Expect(domainSpec.VCPUs.VCPU[1].Hotpluggable).To(Equal("no"), "Expecting the 2nd vcpu to be stable")
				Expect(domainSpec.VCPUs.VCPU[2].Hotpluggable).To(Equal("yes"), "Expecting the 3rd vcpu to be Hotpluggable")
				Expect(domainSpec.VCPUs.VCPU[3].Hotpluggable).To(Equal("yes"), "Expecting the 4th vcpu to be Hotpluggable")
				Expect(domainSpec.VCPUs.VCPU[4].Hotpluggable).To(Equal("yes"), "Expecting the 5th vcpu to be Hotpluggable")
				Expect(domainSpec.VCPUs.VCPU[5].Hotpluggable).To(Equal("yes"), "Expecting the 6th vcpu to be Hotpluggable")
				Expect(domainSpec.VCPUs.VCPU[0].Enabled).To(Equal("yes"), "Expecting the 1st vcpu to be enabled")
				Expect(domainSpec.VCPUs.VCPU[1].Enabled).To(Equal("yes"), "Expecting the 2nd vcpu to be enabled")
				Expect(domainSpec.VCPUs.VCPU[2].Enabled).To(Equal("yes"), "Expecting the 3rd vcpu to be enabled")
				Expect(domainSpec.VCPUs.VCPU[3].Enabled).To(Equal("yes"), "Expecting the 4th vcpu to be enabled")
				Expect(domainSpec.VCPUs.VCPU[4].Enabled).To(Equal("no"), "Expecting the 5th vcpu to be disabled")
				Expect(domainSpec.VCPUs.VCPU[5].Enabled).To(Equal("no"), "Expecting the 6th vcpu to be disabled")
			},
				Entry("on amd64", amd64),
				Entry("on ppc64le", ppc64le),
				Entry("on s390x", s390x),
			)

			It("should not define hotplugable topology for ARM64", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Architecture = arm64
				vmi.Spec.Domain.Machine = &v1.Machine{Type: "virt"}
				vmi.Spec.Domain.CPU = &v1.CPU{
					Cores:      2,
					MaxSockets: 3,
					Sockets:    2,
				}
				c.Architecture = archconverter.NewConverter(arm64)
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
				Expect(domainSpec.CPU.Topology.Cores).To(Equal(uint32(2)), "Expect cores")
				Expect(domainSpec.CPU.Topology.Sockets).To(Equal(uint32(2)), "Expect sockets")
				Expect(domainSpec.CPU.Topology.Threads).To(Equal(uint32(1)), "Expect threads")
				Expect(domainSpec.VCPU.CPUs).To(Equal(uint32(4)), "Expect vcpus")
				Expect(domainSpec.VCPUs).To(BeNil(), "Expecting topology for hotplug")
			})

			DescribeTable("should convert CPU model", func(model string) {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Cores: 3,
					Model: model,
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Mode).To(Equal(model), "Expect mode")
			},
				Entry(v1.CPUModeHostPassthrough, v1.CPUModeHostPassthrough),
				Entry(v1.CPUModeHostModel, v1.CPUModeHostModel),
			)
		})

		Context("when CPU spec defined and model not", func() {
			It("should set host-model CPU mode", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.CPU = &v1.CPU{
					Cores: 3,
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Mode).To(Equal("host-model"))
			})
		})

		Context("when CPU spec not defined", func() {
			It("should set host-model CPU mode", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				Expect(domainSpec.CPU.Mode).To(Equal("host-model"))
			})
		})

		DescribeTable("CPU mpx feature", func(arch string, matcher types.GomegaMatcher) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			c.Architecture = archconverter.NewConverter(arch)
			vmi.Spec.Domain.CPU = &v1.CPU{}
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.CPU.Features).To(matcher)
		},
			Entry("should be nil for s390x", s390x, BeNil()),
			Entry("should be present for amd64", amd64, HaveExactElements(api.CPUFeature{Name: "mpx", Policy: "disable"})),
			Entry("should be nil for arm64", arm64, BeNil()),
		)

		Context("when downwardMetrics are exposed via virtio-serial", func() {
			It("should set socket options", func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.Devices.DownwardMetrics = &v1.DownwardMetrics{}
				domain := vmiToDomain(vmi, c)

				Expect(domain.Spec.Devices.Channels).To(ContainElement(
					api.Channel{
						Type: "unix",
						Source: &api.ChannelSource{
							Mode: "bind",
							Path: downwardmetrics.DownwardMetricsChannelSocket,
						},
						Target: &api.ChannelTarget{
							Type: v1.VirtIO,
							Name: downwardmetrics.DownwardMetricsSerialDeviceName,
						},
					}))
			})
		})

		It("should set disk pci address when specified", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks[0].Disk.PciAddress = "0000:81:01.0"
			test_address := api.Address{
				Type:     api.AddressPCI,
				Domain:   "0x0000",
				Bus:      "0x81",
				Slot:     "0x01",
				Function: "0x0",
			}
			domain := vmiToDomain(vmi, c)
			Expect(*domain.Spec.Devices.Disks[0].Address).To(Equal(test_address))
		})

		It("should generate the block backingstore disk within the domain", func() {
			vmi = libvmi.New(
				libvmi.WithEphemeralPersistentVolumeClaim(blockPVCName, "test-ephemeral"),
			)

			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true, EphemeraldiskCreator: EphemeralDiskImageCreator, IsBlockPVC: isBlockPVCMap, IsBlockDV: isBlockDVMap})
			By("Checking if the disk backing store type is block")
			Expect(domain.Spec.Devices.Disks[0].BackingStore).ToNot(BeNil())
			Expect(domain.Spec.Devices.Disks[0].BackingStore.Type).To(Equal("block"))
			By("Checking if the disk backing store device path is appropriately configured")
			Expect(domain.Spec.Devices.Disks[0].BackingStore.Source.Dev).To(Equal(GetBlockDeviceVolumePath(blockPVCName)))
		})

		It("should fail disk config pci address is set with a non virtio bus", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks[0].Disk.PciAddress = "0000:81:01.0"
			vmi.Spec.Domain.Devices.Disks[0].Disk.Bus = "scsi"
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, &api.Domain{}, c)).ToNot(Succeed())
		})

		It("should succeed with SCSI reservation", func() {
			name := "scsi-reservation"
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks = []v1.Disk{
				{
					Name: name,
					DiskDevice: v1.DiskDevice{
						Disk: nil,
						LUN: &v1.LunTarget{
							Bus:         "scsi",
							Reservation: true,
						},
					},
				}}
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: name,
				VolumeSource: v1.VolumeSource{
					Ephemeral: &v1.EphemeralVolumeSource{
						PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: name,
						},
					},
				},
			})
			c.DisksInfo = make(map[string]*disk.DiskInfo)
			c.DisksInfo[name] = &disk.DiskInfo{}
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			reserv := domainSpec.Devices.Disks[0].Source.Reservations
			Expect(reserv.Managed).To(Equal("no"))
			Expect(reserv.SourceReservations.Type).To(Equal("unix"))
			Expect(reserv.SourceReservations.Path).To(Equal("/var/run/kubevirt/daemons/pr/pr-helper.sock"))
			Expect(reserv.SourceReservations.Mode).To(Equal("client"))
		})

		It("should allow CD-ROM with no volume", func() {
			name := "empty-cdrom"
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
				Name: name,
				DiskDevice: v1.DiskDevice{
					CDRom: &v1.CDRomTarget{
						Bus: v1.DiskBusSATA,
					},
				},
			})
			dom := &api.Domain{}
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, dom, c)).To(Succeed())
			Expect(dom.Spec.Devices.Disks).To(ContainElement(api.Disk{
				Type:     "block",
				Device:   "cdrom",
				Driver:   &api.DiskDriver{Name: "qemu", Type: "raw", ErrorPolicy: "stop", Discard: "unmap"},
				Target:   api.DiskTarget{Bus: "sata", Device: "sda"},
				ReadOnly: &api.ReadOnly{},
				Alias:    api.NewUserDefinedAlias(name),
			}))
		})

		DescribeTable("should add a virtio-scsi controller if a scsci disk is present and iothreads set", func(arch, expectedModel string) {
			c.Architecture = archconverter.NewConverter(arch)
			one := uint(1)
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks[0].Disk.Bus = "scsi"
			dom := &api.Domain{}
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, dom, c)).To(Succeed())
			Expect(dom.Spec.Devices.Controllers).To(ContainElement(api.Controller{
				Type:  "scsi",
				Index: "0",
				Model: expectedModel,
				Driver: &api.ControllerDriver{
					IOThread: &one,
					Queues:   &one,
				},
			}))
		},
			Entry("on amd64", amd64, "virtio-non-transitional"),
			Entry("on arm64", arm64, "virtio-non-transitional"),
			Entry("on ppc64le", ppc64le, "virtio-non-transitional"),
			Entry("on s390x", s390x, "virtio-scsi"),
		)

		DescribeTable("should add a virtio-scsi controller if a scsci disk is present and iothreads NOT set", func(arch, expectedModel string) {
			c.Architecture = archconverter.NewConverter(arch)
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks[0].Disk.Bus = "scsi"
			vmi.Spec.Domain.IOThreadsPolicy = nil
			for i := range vmi.Spec.Domain.Devices.Disks {
				vmi.Spec.Domain.Devices.Disks[i].DedicatedIOThread = nil
			}
			dom := &api.Domain{}
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, dom, c)).To(Succeed())
			Expect(dom.Spec.Devices.Controllers).To(ContainElement(api.Controller{
				Type:  "scsi",
				Index: "0",
				Model: expectedModel,
			}))
		},
			Entry("on amd64", amd64, "virtio-non-transitional"),
			Entry("on arm64", arm64, "virtio-non-transitional"),
			Entry("on ppc64le", ppc64le, "virtio-non-transitional"),
			Entry("on s390x", s390x, "virtio-scsi"),
		)

		It("should not add a virtio-scsi controller if no scsi disk is present", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks[0].Disk.Bus = "sata"
			dom := &api.Domain{}
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, dom, c)).To(Succeed())
			Expect(dom.Spec.Devices.Controllers).ToNot(ContainElement(api.Controller{
				Type:  "scsi",
				Index: "0",
				Model: "virtio-non-transitional",
			}))
		})

		DescribeTable("usb controller", func(arch, bus string, matcher types.GomegaMatcher) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Inputs[0].Bus = v1.InputBus(bus)
			c.Architecture = archconverter.NewConverter(arch)
			domain := vmiToDomain(vmi, c)
			disabled := false
			for _, controller := range domain.Spec.Devices.Controllers {
				if controller.Type == "usb" && controller.Model == "none" {
					disabled = true
				}
			}

			Expect(disabled).To(matcher, "Expect controller not to be disabled")
		},
			Entry("should not be disabled on amd64 usb device is present", amd64, "usb", BeFalse()),
			Entry("should not be disabled on amd64 when device with no bus is present", amd64, "", BeFalse()),
			Entry("should not be disabled on arm64 usb device is present", arm64, "usb", BeFalse()),
			Entry("should not be disabled on arm64 when device with no bus is present", arm64, "", BeFalse()),
			Entry("should not be disabled on ppc64le usb device is present", ppc64le, "usb", BeFalse()),
			Entry("should not be disabled on ppc64le when device with no bus is present", ppc64le, "", BeFalse()),
			Entry("should be disabled on s390x when usb device is present", s390x, "usb", BeTrue()),
			Entry("should be disabled on s390x when device with no bus is present", s390x, "", BeTrue()),
		)

		DescribeTable("PCIHole64 on pcie-root Controller should", func(arch, value string, expected bool) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			if value != "" {
				if vmi.Annotations == nil {
					vmi.Annotations = make(map[string]string)
				}
				vmi.Annotations[v1.DisablePCIHole64] = value
			}
			c.Architecture = archconverter.NewConverter(arch)
			domain := vmiToDomain(vmi, c)

			containElement := ContainElement(api.Controller{
				Type:  "pci",
				Index: "0",
				Model: "pcie-root",
				PCIHole64: &api.PCIHole64{
					Value: 0,
					Unit:  "KiB",
				},
			})
			if expected {
				Expect(domain.Spec.Devices.Controllers).To(containElement, "Expected pcihole64 to be set to zero")
			} else {
				Expect(domain.Spec.Devices.Controllers).ToNot(containElement, "Expected pcihole64 to not be set")
			}
		},
			Entry("be set to zero on amd64 when annotation was set to true", amd64, "true", true),
			Entry("not be set on amd64 when annotation was not set", amd64, "", false),
			Entry("not be set on amd64 when annotation was set not to true", amd64, "something", false),
			Entry("not be set on arm64 when annotation was set to true", arm64, "true", false),
			Entry("not be set on arm64 when annotation was not set", arm64, "", false),
			Entry("not be set on arm64 when annotation was set not to true", arm64, "something", false),
			Entry("not be set on ppc64le when annotation was set to true", ppc64le, "true", false),
			Entry("not be set on ppc64le when annotation was not set", ppc64le, "", false),
			Entry("not be set on ppc64le when annotation was set not to true", ppc64le, "something", false),
			Entry("not be set on s390x when annotation was set to true", s390x, "true", false),
			Entry("not be set on s390x when annotation was not set", s390x, "", false),
			Entry("not be set on s390x when annotation was set not to true", s390x, "something", false),
		)

		It("should fail when input device is set to ps2 bus", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Inputs[0].Bus = "ps2"
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, &api.Domain{}, c)).ToNot(Succeed(), "Expect error")
		})

		It("should fail when input device is set to keyboard type", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Inputs[0].Type = "keyboard"
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, &api.Domain{}, c)).ToNot(Succeed(), "Expect error")
		})

		It("should succeed when input device is set to usb bus", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Inputs[0].Bus = "usb"
			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, &api.Domain{}, c)).To(Succeed(), "Expect success")
		})

		It("should succeed when input device bus is empty", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Inputs[0].Bus = ""
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.Inputs[0].Bus).To(Equal(v1.InputBusUSB), "Expect usb bus")
		})

		It("should not overwrite the IO policy when when IO threads are enabled", func() {
			ioPolicy := v1.IONative
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Volumes[0] = v1.Volume{
				Name: "disk",
				VolumeSource: v1.VolumeSource{
					Ephemeral: &v1.EphemeralVolumeSource{
						PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testclaim",
						},
					},
				},
			}
			vmi.Spec.Domain.Devices.Disks[0] = v1.Disk{
				Name:              "disk",
				DedicatedIOThread: pointer.P(true),
				IO:                ioPolicy,
			}
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.Disks[0].Driver.IO).To(Equal(ioPolicy))
		})

		It("should not enable sound cards emulation by default", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Sound = nil
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.SoundCards).To(BeEmpty())
		})

		It("should enable default sound card with existing but empty sound devices", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			name := "audio-default-ich9"
			vmi.Spec.Domain.Devices.Sound = &v1.SoundDevice{
				Name: name,
			}
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.SoundCards).To(HaveLen(1))
			Expect(domain.Spec.Devices.SoundCards).To(ContainElement(api.SoundCard{
				Alias: api.NewUserDefinedAlias(name),
				Model: "ich9",
			}))
		})

		It("should enable ac97 sound card ", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			name := "audio-ac97"
			vmi.Spec.Domain.Devices.Sound = &v1.SoundDevice{
				Name:  name,
				Model: "ac97",
			}
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.SoundCards).To(HaveLen(1))
			Expect(domain.Spec.Devices.SoundCards).To(ContainElement(api.SoundCard{
				Alias: api.NewUserDefinedAlias(name),
				Model: "ac97",
			}))
		})

		DescribeTable("usb redirection", func(arch string, expectedModel string) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.ClientPassthrough = &v1.ClientPassthroughDevices{}
			c.Architecture = archconverter.NewConverter(arch)
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.Redirs).To(HaveLen(4))
			Expect(domain.Spec.Devices.Controllers).To(ContainElement(api.Controller{
				Type:  "usb",
				Index: "0",
				Model: expectedModel,
			}))
		},
			Entry("should be enabled on amd64 when number of USB client devices > 0", amd64, "qemu-xhci"),
			Entry("should be enabled on ppc64le ", ppc64le, "qemu-xhci"),
			Entry("should be enabled on arm64 ", arm64, "qemu-xhci"),
			Entry("should be disabled on s390x", s390x, "none"),
		)

		It("should not enable usb redirection when numberOfDevices == 0", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.ClientPassthrough = nil
			c.Architecture = archconverter.NewConverter(amd64)
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.Redirs).To(BeNil())
			Expect(domain.Spec.Devices.Controllers).ToNot(ContainElement(api.Controller{
				Type:  "usb",
				Index: "0",
				Model: "qemu-xhci",
			}))
		})

		It("should select explicitly chosen network model", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Interfaces[0].Model = "e1000"
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.Interfaces[0].Model.Type).To(Equal("e1000"))
		})

		It("should set rom to off when no boot order is specified", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Interfaces[0].BootOrder = nil
			domain := vmiToDomain(vmi, c)
			Expect(domain.Spec.Devices.Interfaces[0].Rom.Enabled).To(Equal("no"))
		})

		When("NIC PCI address is specified on VMI", func() {
			const pciAddress = "0000:81:01.0"
			expectedPCIAddress := api.Address{
				Type:     api.AddressPCI,
				Domain:   "0x0000",
				Bus:      "0x81",
				Slot:     "0x01",
				Function: "0x0",
			}

			BeforeEach(func() {
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			})

			It("should be set on the domain spec for a non-SRIOV nic", func() {
				vmi.Spec.Domain.Devices.Interfaces[0].PciAddress = pciAddress
				domain := vmiToDomain(vmi, c)
				Expect(*domain.Spec.Devices.Interfaces[0].Address).To(Equal(expectedPCIAddress))

			})
		})

		DescribeTable("should calculate mebibyte from a quantity", func(quantity string, mebibyte int) {
			mi64, _ := resource.ParseQuantity(quantity)
			Expect(vcpu.QuantityToMebiByte(mi64)).To(BeNumerically("==", mebibyte))
		},
			Entry("when 0M is given", "0M", 0),
			Entry("when 0 is given", "0", 0),
			Entry("when 1 is given", "1", 1),
			Entry("when 1M is given", "1M", 1),
			Entry("when 3M is given", "3M", 3),
			Entry("when 100M is given", "100M", 95),
			Entry("when 1Mi is given", "1Mi", 1),
			Entry("when 2G are given", "2G", 1907),
			Entry("when 2Gi are given", "2Gi", 2*1024),
			Entry("when 2780Gi are given", "2780Gi", 2780*1024),
		)

		It("should fail calculating mebibyte if the quantity is less than 0", func() {
			mi64, _ := resource.ParseQuantity("-2G")
			_, err := vcpu.QuantityToMebiByte(mi64)
			Expect(err).To(HaveOccurred())
		})

		DescribeTable("should calculate memory in bytes", func(quantity string, bytes int) {
			m64, _ := resource.ParseQuantity(quantity)
			memory, err := vcpu.QuantityToByte(m64)
			Expect(memory.Value).To(BeNumerically("==", bytes))
			Expect(memory.Unit).To(Equal("b"))
			Expect(err).ToNot(HaveOccurred())
		},
			Entry("specifying memory 64M", "64M", 64*1000*1000),
			Entry("specifying memory 64Mi", "64Mi", 64*1024*1024),
			Entry("specifying memory 3G", "3G", 3*1000*1000*1000),
			Entry("specifying memory 3Gi", "3Gi", 3*1024*1024*1024),
			Entry("specifying memory 45Gi", "45Gi", 45*1024*1024*1024),
			Entry("specifying memory 2780Gi", "2780Gi", 2780*1024*1024*1024),
			Entry("specifying memory 451231 bytes", "451231", 451231),
		)
		It("should calculate memory in bytes", func() {
			By("specyfing negative memory size -45Gi")
			m45gi, _ := resource.ParseQuantity("-45Gi")
			_, err := vcpu.QuantityToByte(m45gi)
			Expect(err).To(HaveOccurred())
		})

		It("should convert hugepages", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Memory = &v1.Memory{
				Hugepages: &v1.Hugepages{},
			}
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(domainSpec.MemoryBacking.HugePages).ToNot(BeNil())
			Expect(domainSpec.MemoryBacking.Source).ToNot(BeNil())
			Expect(domainSpec.MemoryBacking.Source.Type).To(Equal("memfd"))

			Expect(domainSpec.Memory.Value).To(Equal(uint64(8388608)))
			Expect(domainSpec.Memory.Unit).To(Equal("b"))
		})

		It("should use guest memory instead of requested memory if present", func() {
			guestMemory := resource.MustParse("123Mi")
			vmi.Spec.Domain.Memory = &v1.Memory{
				Guest: &guestMemory,
			}
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)

			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

			Expect(domainSpec.Memory.Value).To(Equal(uint64(128974848)))
			Expect(domainSpec.Memory.Unit).To(Equal("b"))
		})

		It("should not add RNG when not present", func() {
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(domainSpec.Devices.Rng).To(BeNil())
		})

		It("should add RNG when present", func() {
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(domainSpec.Devices.Rng).ToNot(BeNil())
		})

		DescribeTable("Validate that QEMU SeaBios debug logs are ",
			func(toDefineVerbosityEnvVariable bool, virtLauncherLogVerbosity int, shouldEnableDebugLogs bool) {

				if toDefineVerbosityEnvVariable {
					Expect(os.Setenv(services.ENV_VAR_VIRT_LAUNCHER_LOG_VERBOSITY, strconv.Itoa(virtLauncherLogVerbosity))).
						To(Succeed())
					defer func() {
						Expect(os.Unsetenv(services.ENV_VAR_VIRT_LAUNCHER_LOG_VERBOSITY)).To(Succeed())
					}()
				}

				domain := api.Domain{}

				Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, &domain, c)).To(Succeed())

				if domain.Spec.QEMUCmd == nil || (domain.Spec.QEMUCmd.QEMUArg == nil) {
					return
				}

				if shouldEnableDebugLogs {
					Expect(domain.Spec.QEMUCmd.QEMUArg).Should(ContainElements(
						api.Arg{Value: "-chardev"},
						api.Arg{Value: "file,id=firmwarelog,path=/tmp/qemu-firmware.log"},
						api.Arg{Value: "-device"},
						api.Arg{Value: "isa-debugcon,iobase=0x402,chardev=firmwarelog"},
					))
				} else {
					Expect(domain.Spec.QEMUCmd.QEMUArg).ShouldNot(Or(
						ContainElements(api.Arg{Value: "-chardev"}),
						ContainElements(api.Arg{Value: "file,id=firmwarelog,path=/tmp/qemu-firmware.log"}),
						ContainElements(api.Arg{Value: "-device"}),
						ContainElements(api.Arg{Value: "isa-debugcon,iobase=0x402,chardev=firmwarelog"}),
					))
				}

			},
			Entry("disabled - virtLauncherLogVerbosity does not exceed verbosity threshold", true, 0, false),
			Entry("enabled - virtLaucherLogVerbosity exceeds verbosity threshold", true, 1, true),
			Entry("disabled - virtLauncherLogVerbosity variable is not defined", false, -1, false),
		)

		DescribeTable("should add VSOCK section when present",
			func(useVirtioTransitional bool) {
				vmi.Status.VSOCKCID = pointer.P(uint32(100))
				vmi.Spec.Domain.Devices.AutoattachVSOCK = pointer.P(true)
				c.UseVirtioTransitional = useVirtioTransitional
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
				Expect(domainSpec.Devices.VSOCK).ToNot(BeNil())
				Expect(domainSpec.Devices.VSOCK.Model).To(Equal("virtio-non-transitional"))
				Expect(domainSpec.Devices.VSOCK.CID.Auto).To(Equal("no"))
				Expect(domainSpec.Devices.VSOCK.CID.Address).To(BeNumerically("==", 100))
			},
			Entry("use virtio transitional", true),
			Entry("use virtio non-transitional", false),
		)
		DescribeTable("Should set the error policy", func(epolicy *v1.DiskErrorPolicy, expected string) {
			vmi.Spec.Domain.Devices.Disks[0] = v1.Disk{
				Name: "mydisk",
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{
						Bus: v1.VirtIO,
					},
				},
				ErrorPolicy: epolicy,
			}
			vmi.Spec.Volumes[0] = v1.Volume{
				Name: "mydisk",
				VolumeSource: v1.VolumeSource{
					Ephemeral: &v1.EphemeralVolumeSource{
						PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testclaim",
						},
					},
				},
			}
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(string(domainSpec.Devices.Disks[0].Driver.ErrorPolicy)).To(Equal(expected))
		},
			Entry("ErrorPolicy not specified", nil, "stop"),
			Entry("ErrorPolicy equal to stop", pointer.P(v1.DiskErrorPolicyStop), "stop"),
			Entry("ErrorPolicy equal to ignore", pointer.P(v1.DiskErrorPolicyIgnore), "ignore"),
			Entry("ErrorPolicy equal to report", pointer.P(v1.DiskErrorPolicyReport), "report"),
			Entry("ErrorPolicy equal to enospace", pointer.P(v1.DiskErrorPolicyEnospace), "enospace"),
		)
		DescribeTable("Should set the vmport by arch", func(arch string) {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			c.Architecture = archconverter.NewConverter(arch)
			domain := vmiToDomain(vmi, c)
			switch arch {
			case amd64:
				Expect(domain.Spec.Features.VMPort.State).To(Equal("off"))
			case arm64, s390x, ppc64le:
				Expect(domain.Spec.Features.VMPort).To(BeNil())
			}
		},
			MultiArchEntry(""),
		)
	})
	Context("Network convert", func() {
		var vmi *v1.VirtualMachineInstance
		var c *ConverterContext

		const netName1 = "red1"
		const netName2 = "red2"

		BeforeEach(func() {
			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "mynamespace",
				},
			}

			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				VirtualMachine: vmi,
				Secrets: map[string]*k8sv1.Secret{
					"mysecret": {
						Data: map[string][]byte{
							"node.session.auth.username": []byte("admin"),
						},
					},
				},
				AllowEmulation: true,
				SMBios:         TestSmbios,
				DomainAttachmentByInterfaceName: map[string]string{
					"default": string(v1.Tap),
					netName1:  string(v1.Tap),
					netName2:  string(v1.Tap),
				},
			}
		})
		It("Should set domain interface state down", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{
				*v1.DefaultBridgeNetworkInterface(),
			}
			vmi.Spec.Domain.Devices.Interfaces[0].State = v1.InterfaceStateLinkDown
			vmi.Spec.Networks = []v1.Network{*v1.DefaultPodNetwork()}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(1))
			Expect(domain.Spec.Devices.Interfaces[0].LinkState.State).To(Equal("down"))
		})
		It("Should set domain interface source correctly for multus", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{
				*v1.DefaultBridgeNetworkInterface(),
				*v1.DefaultBridgeNetworkInterface(),
				*v1.DefaultBridgeNetworkInterface(),
			}
			vmi.Spec.Domain.Devices.Interfaces[0].Name = netName1
			vmi.Spec.Domain.Devices.Interfaces[1].Name = netName2
			// 3rd network is the default pod network, name is "default"
			vmi.Spec.Networks = []v1.Network{
				{
					Name: netName1,
					NetworkSource: v1.NetworkSource{
						Multus: &v1.MultusNetwork{NetworkName: "red"},
					},
				},
				{
					Name: netName2,
					NetworkSource: v1.NetworkSource{
						Multus: &v1.MultusNetwork{NetworkName: "red"},
					},
				},
				{
					Name: "default",
					NetworkSource: v1.NetworkSource{
						Pod: &v1.PodNetwork{},
					},
				},
			}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(3))
			Expect(domain.Spec.Devices.Interfaces[0].Type).To(Equal("ethernet"))
			Expect(domain.Spec.Devices.Interfaces[1].Type).To(Equal("ethernet"))
			Expect(domain.Spec.Devices.Interfaces[2].Type).To(Equal("ethernet"))
		})
		It("Should set domain interface source correctly for default multus", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{
				*v1.DefaultBridgeNetworkInterface(),
				*v1.DefaultBridgeNetworkInterface(),
			}
			vmi.Spec.Domain.Devices.Interfaces[0].Name = netName1
			vmi.Spec.Domain.Devices.Interfaces[1].Name = netName2
			vmi.Spec.Networks = []v1.Network{
				{
					Name: netName1,
					NetworkSource: v1.NetworkSource{
						Multus: &v1.MultusNetwork{NetworkName: "red", Default: true},
					},
				},
				{
					Name: netName2,
					NetworkSource: v1.NetworkSource{
						Multus: &v1.MultusNetwork{NetworkName: "red"},
					},
				},
			}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(2))
			Expect(domain.Spec.Devices.Interfaces[0].Type).To(Equal("ethernet"))
			Expect(domain.Spec.Devices.Interfaces[1].Type).To(Equal("ethernet"))
		})
		It("should allow setting boot order", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			iface1 := v1.DefaultBridgeNetworkInterface()
			iface2 := v1.DefaultBridgeNetworkInterface()
			net1 := v1.DefaultPodNetwork()
			net2 := v1.DefaultPodNetwork()
			iface1.Name = netName1
			iface2.Name = netName2
			bootOrder := uint(1)
			iface1.BootOrder = &bootOrder
			net1.Name = netName1
			net2.Name = netName2
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{*iface1, *iface2}
			vmi.Spec.Networks = []v1.Network{*net1, *net2}
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(2))
			Expect(domain.Spec.Devices.Interfaces[0].BootOrder).NotTo(BeNil())
			Expect(domain.Spec.Devices.Interfaces[0].BootOrder.Order).To(Equal(bootOrder))
			Expect(domain.Spec.Devices.Interfaces[1].BootOrder).To(BeNil())
		})
		It("Should create network configuration for masquerade interface", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)

			iface1 := v1.Interface{Name: netName1, InterfaceBindingMethod: v1.InterfaceBindingMethod{Masquerade: &v1.InterfaceMasquerade{}}}
			net1 := v1.DefaultPodNetwork()
			net1.Name = netName1

			vmi.Spec.Networks = []v1.Network{*net1}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{iface1}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(1))
			Expect(domain.Spec.Devices.Interfaces[0].Type).To(Equal("ethernet"))
		})
		It("Should create network configuration for masquerade interface and the pod network and a secondary network using multus", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)

			iface1 := v1.Interface{Name: netName1, InterfaceBindingMethod: v1.InterfaceBindingMethod{Masquerade: &v1.InterfaceMasquerade{}}}
			net1 := v1.DefaultPodNetwork()
			net1.Name = netName1

			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{iface1, *v1.DefaultBridgeNetworkInterface()}
			vmi.Spec.Domain.Devices.Interfaces[1].Name = "red1"

			vmi.Spec.Networks = []v1.Network{*net1,
				{
					Name: "red1",
					NetworkSource: v1.NetworkSource{
						Multus: &v1.MultusNetwork{NetworkName: "red"},
					},
				}}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(2))
			Expect(domain.Spec.Devices.Interfaces[0].Type).To(Equal("ethernet"))
			Expect(domain.Spec.Devices.Interfaces[1].Type).To(Equal("ethernet"))
		})
		It("Should create network configuration for an interface using a binding plugin with tap domain attachment", func() {
			bindingName := "BindingName"
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)

			iface1 := v1.Interface{Name: netName1, Binding: &v1.PluginBinding{Name: bindingName}}
			net1 := v1.DefaultPodNetwork()
			net1.Name = netName1

			vmi.Spec.Networks = []v1.Network{*net1}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{iface1}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(1))
			Expect(domain.Spec.Devices.Interfaces[0].Type).To(Equal("ethernet"))
		})
		It("Shouldn't create network configuration for an interface using a binding plugin with non-tap domain attachment", func() {
			bindingName := "BindingName"
			c.DomainAttachmentByInterfaceName[bindingName] = "non-tap"
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			name1 := "Name"

			iface1 := v1.Interface{Name: name1, Binding: &v1.PluginBinding{Name: bindingName}}
			net1 := v1.DefaultPodNetwork()
			net1.Name = name1

			vmi.Spec.Networks = []v1.Network{*net1}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{iface1}

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(BeEmpty())
		})
		It("creates SRIOV hostdev", func() {
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			domain := &api.Domain{}

			const identifyDevice = "sriov-test"
			c.SRIOVDevices = append(c.SRIOVDevices, api.HostDevice{Type: identifyDevice})

			Expect(Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, domain, c)).To(Succeed())
			Expect(domain.Spec.Devices.HostDevices).To(Equal([]api.HostDevice{{Type: identifyDevice}}))
		})
	})

	Context("graphics and video device", func() {

		DescribeTable("should check autoAttachGraphicsDevices", func(arch string, autoAttach *bool, devices int) {

			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						CPU: &v1.CPU{Cores: 3},
						Resources: v1.ResourceRequirements{
							Requests: k8sv1.ResourceList{
								k8sv1.ResourceCPU:    resource.MustParse("1m"),
								k8sv1.ResourceMemory: resource.MustParse("64M"),
							},
						},
					},
				},
			}
			vmi.Spec.Domain.Devices = v1.Devices{
				AutoattachGraphicsDevice: autoAttach,
			}
			domain := vmiToDomain(&vmi, &ConverterContext{AllowEmulation: true, Architecture: archconverter.NewConverter(arch)})
			Expect(domain.Spec.Devices.Video).To(HaveLen(devices))
			Expect(domain.Spec.Devices.Graphics).To(HaveLen(devices))

			if autoAttach == nil || *autoAttach {
				switch arch {
				case amd64, ppc64le:
					Expect(domain.Spec.Devices.Video[0].Model.Type).To(Equal("vga"))
					Expect(domain.Spec.Devices.Inputs).To(BeEmpty())
				case arm64:
					Expect(domain.Spec.Devices.Video[0].Model.Type).To(Equal(v1.VirtIO))
					Expect(domain.Spec.Devices.Inputs[0].Type).To(Equal(v1.InputTypeTablet))
					Expect(domain.Spec.Devices.Inputs[1].Type).To(Equal(v1.InputTypeKeyboard))
				case s390x:
					Expect(domain.Spec.Devices.Video[0].Model.Type).To(Equal(v1.VirtIO))
					Expect(domain.Spec.Devices.Inputs[0].Type).To(Equal(v1.InputTypeKeyboard))
					Expect(domain.Spec.Devices.Inputs[0].Bus).To(Equal(v1.InputBusVirtio))
				}
			}
		},
			MultiArchEntry("and add the graphics and video device if it is not set", nil, 1),
			MultiArchEntry("and add the graphics and video device if it is set to true", pointer.P(true), 1),
			MultiArchEntry("and not add the graphics and video device if it is set to false", pointer.P(false), 0),
		)

		DescribeTable("should check video device", func(arch string) {
			const expectedVideoType = "test-video"
			vmi := libvmi.New(libvmi.WithAutoattachGraphicsDevice(true), libvmi.WithVideo(expectedVideoType))
			domain := vmiToDomain(vmi, &ConverterContext{AllowEmulation: true, Architecture: archconverter.NewConverter(arch)})
			Expect(domain.Spec.Devices.Video[0].Model.Type).To(Equal(expectedVideoType))
		},
			MultiArchEntry("and use the explicitly set video device"),
		)

		DescribeTable("Should have one vnc", func(arch string) {
			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{},
				},
			}

			domain := vmiToDomain(&vmi, &ConverterContext{Architecture: archconverter.NewConverter(arch), AllowEmulation: true})
			Expect(domain.Spec.Devices.Graphics).To(HaveLen(1))
			Expect(domain.Spec.Devices.Graphics).To(HaveExactElements(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
				"Type": Equal("vnc"),
			})))
		},
			MultiArchEntry(""),
		)
	})

	Context("HyperV", func() {
		DescribeTable("should convert hyperv features", func(hyperV *v1.FeatureHyperv, result *api.FeatureHyperv) {
			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						Features: &v1.Features{
							Hyperv: hyperV,
						},
					},
				},
			}

			domain := vmiToDomain(&vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			Expect(domain.Spec.Features.Hyperv).To(Equal(result))

		},
			Entry("and add the vapic feature", &v1.FeatureHyperv{VAPIC: &v1.FeatureState{}}, &api.FeatureHyperv{VAPIC: &api.FeatureState{State: "on"}}),
			Entry("and add the stimer direct feature", &v1.FeatureHyperv{
				SyNICTimer: &v1.SyNICTimer{
					Direct: &v1.FeatureState{},
				},
			}, &api.FeatureHyperv{
				SyNICTimer: &api.SyNICTimer{
					State:  "on",
					Direct: &api.FeatureState{State: "on"},
				},
			}),
			Entry("and add the stimer feature without direct", &v1.FeatureHyperv{
				SyNICTimer: &v1.SyNICTimer{},
			}, &api.FeatureHyperv{
				SyNICTimer: &api.SyNICTimer{
					State: "on",
				},
			}),
			Entry("and add the vapic and the stimer direct feature", &v1.FeatureHyperv{
				SyNICTimer: &v1.SyNICTimer{
					Direct: &v1.FeatureState{},
				},
				VAPIC: &v1.FeatureState{},
			}, &api.FeatureHyperv{
				SyNICTimer: &api.SyNICTimer{
					State:  "on",
					Direct: &api.FeatureState{State: "on"},
				},
				VAPIC: &api.FeatureState{State: "on"},
			}),
		)

		It("should convert hyperv passthrough", func() {
			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						Features: &v1.Features{
							HypervPassthrough: &v1.HyperVPassthrough{Enabled: pointer.P(true)},
						},
					},
				},
			}

			domain := vmiToDomain(&vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			Expect(domain.Spec.Features.Hyperv.Mode).To(Equal(api.HypervModePassthrough))
		})
	})

	Context("serial console", func() {

		DescribeTable("should check autoAttachSerialConsole", func(autoAttach *bool, devices int) {

			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						CPU: &v1.CPU{Cores: 3},
						Resources: v1.ResourceRequirements{
							Requests: k8sv1.ResourceList{
								k8sv1.ResourceCPU:    resource.MustParse("1m"),
								k8sv1.ResourceMemory: resource.MustParse("64M"),
							},
						},
					},
				},
			}
			vmi.Spec.Domain.Devices = v1.Devices{
				AutoattachSerialConsole: autoAttach,
			}
			domain := vmiToDomain(&vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			Expect(domain.Spec.Devices.Serials).To(HaveLen(devices))
			Expect(domain.Spec.Devices.Consoles).To(HaveLen(devices))

		},
			Entry("and add the serial console if it is not set", nil, 1),
			Entry("and add the serial console if it is set to true", pointer.P(true), 1),
			Entry("and not add the serial console if it is set to false", pointer.P(false), 0),
		)
	})

	It("should not include serial entry in sysinfo when firmware.serial is not set", func() {
		vmi := libvmi.New()
		v1.SetObjectDefaults_VirtualMachineInstance(vmi)
		domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
		Expect(domain.Spec.SysInfo.System).ToNot(ContainElement(HaveField("Name", Equal("serial"))),
			"serial entry should not be present in sysinfo",
		)
	})

	Context("IOThreads", func() {

		DescribeTable("Should use correct IOThreads policies", func(policy v1.IOThreadsPolicy, cpuCores int, threadCount int, threadIDs []int) {
			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						IOThreadsPolicy: &policy,
						Resources: v1.ResourceRequirements{
							Requests: k8sv1.ResourceList{
								k8sv1.ResourceCPU: resource.MustParse(fmt.Sprintf("%d", cpuCores)),
							},
						},
						Devices: v1.Devices{
							Disks: []v1.Disk{
								{
									Name: "dedicated",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
									DedicatedIOThread: pointer.P(true),
								},
								{
									Name: "shared",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
									DedicatedIOThread: pointer.P(false),
								},
								{
									Name: "omitted1",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
								},
								{
									Name: "omitted2",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
								},
								{
									Name: "omitted3",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
								},
								{
									Name: "omitted4",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
								},
								{
									Name: "omitted5",
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "dedicated",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
						{
							Name: "shared",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
						{
							Name: "omitted1",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
						{
							Name: "omitted2",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
						{
							Name: "omitted3",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
						{
							Name: "omitted4",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
						{
							Name: "omitted5",
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: "testclaim",
									},
								},
							},
						},
					},
				},
			}

			domain := vmiToDomain(&vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true, EphemeraldiskCreator: EphemeralDiskImageCreator})
			Expect(domain.Spec.IOThreads).ToNot(BeNil())
			Expect(int(domain.Spec.IOThreads.IOThreads)).To(Equal(threadCount))
			for idx, disk := range domain.Spec.Devices.Disks {
				Expect(disk.Driver.IOThread).ToNot(BeNil())
				Expect(int(*disk.Driver.IOThread)).To(Equal(threadIDs[idx]))
			}
		},
			Entry("using a shared policy with 1 CPU", v1.IOThreadsPolicyShared, 1, 2, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using a shared policy with 2 CPUs", v1.IOThreadsPolicyShared, 2, 2, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using a shared policy with 3 CPUs", v1.IOThreadsPolicyShared, 2, 2, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using an auto policy with 1 CPU", v1.IOThreadsPolicyAuto, 1, 2, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using an auto policy with 2 CPUs", v1.IOThreadsPolicyAuto, 2, 4, []int{4, 1, 2, 3, 1, 2, 3}),
			Entry("using an auto policy with 3 CPUs", v1.IOThreadsPolicyAuto, 3, 6, []int{6, 1, 2, 3, 4, 5, 1}),
			Entry("using an auto policy with 4 CPUs", v1.IOThreadsPolicyAuto, 4, 7, []int{7, 1, 2, 3, 4, 5, 6}),
			Entry("using an auto policy with 5 CPUs", v1.IOThreadsPolicyAuto, 5, 7, []int{7, 1, 2, 3, 4, 5, 6}),
		)

		It("Should not add IOThreads to non-virtio disks", func() {
			dedicatedDiskName := "dedicated"
			sharedDiskName := "shared"
			incompatibleDiskName := "incompatible"
			claimName := "claimName"
			vmi := v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						Devices: v1.Devices{
							Disks: []v1.Disk{
								{
									Name: dedicatedDiskName,
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
									DedicatedIOThread: pointer.P(true),
								},
								{
									Name: sharedDiskName,
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.VirtIO,
										},
									},
									DedicatedIOThread: pointer.P(false),
								},
								{
									Name: incompatibleDiskName,
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{
											Bus: v1.DiskBusSATA,
										},
									},
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: dedicatedDiskName,
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: claimName,
									},
								},
							},
						},
						{
							Name: sharedDiskName,
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: claimName,
									},
								},
							},
						},
						{
							Name: incompatibleDiskName,
							VolumeSource: v1.VolumeSource{
								Ephemeral: &v1.EphemeralVolumeSource{
									PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
										ClaimName: claimName,
									},
								},
							},
						},
					},
				},
			}

			domain := vmiToDomain(&vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true, EphemeraldiskCreator: EphemeralDiskImageCreator})
			Expect(domain.Spec.IOThreads).ToNot(BeNil())
			Expect(domain.Spec.IOThreads.IOThreads).To(Equal(uint(2)))
			// Disk with dedicated IOThread (2)
			Expect(domain.Spec.Devices.Disks[0].Driver.IOThread).ToNot(BeNil())
			Expect(*domain.Spec.Devices.Disks[0].Driver.IOThread).To(Equal(uint(2)))
			// Disk with shared IOThread
			Expect(domain.Spec.Devices.Disks[1].Driver.IOThread).ToNot(BeNil())
			Expect(*domain.Spec.Devices.Disks[1].Driver.IOThread).To(Equal(uint(1)))
			// Disk incompatible with IOThreads
			Expect(domain.Spec.Devices.Disks[2].Driver.IOThread).To(BeNil())
		})

		It("Should set the iothread pool with the supplementalPool policy", func() {
			count := uint32(4)
			vmi := libvmi.New(
				libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicySupplementalPool),
				libvmi.WithIOThreads(v1.DiskIOThreads{SupplementalPoolThreadCount: pointer.P(count)}),
				libvmi.WithPersistentVolumeClaim("disk0", "pvc0", libvmi.WithDedicatedIOThreads(true)),
			)
			iothreads := &api.DiskIOThreads{}
			for id := 1; id <= int(count); id++ {
				iothreads.IOThread = append(iothreads.IOThread, api.DiskIOThread{Id: uint32(id)})
			}

			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true, EphemeraldiskCreator: EphemeralDiskImageCreator})

			Expect(domain.Spec.IOThreads.IOThreads).To(Equal(uint(count)))
			Expect(domain.Spec.Devices.Disks[0].Driver.IOThreads).To(Equal(iothreads))
		})
	})

	Context("virtio block multi-queue", func() {
		var vmi *v1.VirtualMachineInstance
		var context *ConverterContext

		BeforeEach(func() {
			context = &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), UseVirtioTransitional: false}
			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "mynamespace",
				},
			}
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks = []v1.Disk{
				{
					Name: "mydisk",
					DiskDevice: v1.DiskDevice{
						Disk: &v1.DiskTarget{
							Bus: v1.VirtIO,
						},
					},
				},
			}
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "mydisk",
					VolumeSource: v1.VolumeSource{
						HostDisk: &v1.HostDisk{
							Path:     "/var/run/kubevirt-private/vmi-disks/myvolume/disk.img",
							Type:     v1.HostDiskExistsOrCreate,
							Capacity: resource.MustParse("1Gi"),
						},
					},
				},
			}

			vmi.Spec.Domain.Devices.BlockMultiQueue = pointer.P(true)
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{
				k8sv1.ResourceMemory: resource.MustParse("8192Ki"),
				k8sv1.ResourceCPU:    resource.MustParse("2"),
			}
		})

		It("should assign queues to a device if requested", func() {
			expectedQueues := uint(2)

			v1Disk := v1.Disk{
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{Bus: v1.VirtIO},
				},
			}
			apiDisk := api.Disk{}
			devicePerBus := map[string]deviceNamer{}
			numQueues := uint(2)
			Convert_v1_Disk_To_api_Disk(context, &v1Disk, &apiDisk, devicePerBus, &numQueues, make(map[string]v1.VolumeStatus))
			Expect(apiDisk.Device).To(Equal("disk"), "expected disk device to be defined")
			Expect(*(apiDisk.Driver.Queues)).To(Equal(expectedQueues), "expected queues to be 2")
		})

		It("should not assign queues to a device if omitted", func() {
			v1Disk := v1.Disk{
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{},
				},
			}
			apiDisk := api.Disk{}
			devicePerBus := map[string]deviceNamer{}
			Expect(Convert_v1_Disk_To_api_Disk(context, &v1Disk, &apiDisk, devicePerBus, nil, make(map[string]v1.VolumeStatus))).
				To(Succeed())
			Expect(apiDisk.Device).To(Equal("disk"), "expected disk device to be defined")
			Expect(apiDisk.Driver.Queues).To(BeNil(), "expected no queues to be requested")
		})

		It("should assign correct number of queues with CPU hotplug topology", func() {
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{}
			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores:      2,
				Threads:    2,
				Sockets:    2,
				MaxSockets: 8,
			}
			expectedNumOfBlkQueues :=
				vmi.Spec.Domain.CPU.Cores * vmi.Spec.Domain.CPU.Threads * vmi.Spec.Domain.CPU.Sockets

			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true, SMBios: &cmdv1.SMBios{}})
			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]
			Expect(disk.Driver.Queues).ToNot(BeNil())
			Expect(*disk.Driver.Queues).To(Equal(uint(expectedNumOfBlkQueues)))
		})

		It("should honor multiQueue setting", func() {
			var expectedQueues uint = 2
			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores: 2,
			}

			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true, SMBios: &cmdv1.SMBios{}})
			Expect(*(domain.Spec.Devices.Disks[0].Driver.Queues)).To(Equal(expectedQueues),
				"expected number of queues to equal number of requested vCPUs")
		})
	})

	Context("Correctly handle IsolateEmulatorThread with dedicated cpus", func() {
		DescribeTable("should succeed assigning CPUs to emulatorThread",
			func(cpu v1.CPU, converterContext *ConverterContext, vmiAnnotations map[string]string, expectedEmulatorThreads int) {
				var err error
				domain := &api.Domain{}

				cpuPool := vcpu.NewRelaxedCPUPool(
					&api.CPUTopology{Sockets: cpu.Sockets, Cores: cpu.Cores, Threads: cpu.Threads},
					converterContext.Topology,
					converterContext.CPUSet,
				)
				domain.Spec.CPUTune, err = cpuPool.FitCores()
				Expect(err).ToNot(HaveOccurred())

				vCPUs := hardware.GetNumberOfVCPUs(&cpu)
				emulatorThreadsCPUSet, err := vcpu.FormatEmulatorThreadPin(cpuPool, vmiAnnotations, vCPUs)
				Expect(err).ToNot(HaveOccurred())
				By("checking that the housekeeping CPUSet has the expected amount of CPUs")
				housekeepingCPUs, err := hardware.ParseCPUSetLine(emulatorThreadsCPUSet, 100)
				Expect(err).ToNot(HaveOccurred())
				Expect(housekeepingCPUs).To(HaveLen(expectedEmulatorThreads))
				By("checking that the housekeeping CPUSet does not overlap with the VCPUs CPUSet")
				for _, VCPUPin := range domain.Spec.CPUTune.VCPUPin {
					CPUTuneCPUs, err := hardware.ParseCPUSetLine(VCPUPin.CPUSet, 100)
					Expect(err).ToNot(HaveOccurred())
					Expect(CPUTuneCPUs).ToNot(ContainElements(housekeepingCPUs))
				}
			},
			Entry("when EmulatorThreadCompleteToEvenParity is disabled and there is one extra CPU assigned for emulatorThread",
				v1.CPU{Sockets: 1, Cores: 2, Threads: 1},
				&ConverterContext{CPUSet: []int{5, 6, 7},
					Topology: &cmdv1.Topology{
						NumaCells: []*cmdv1.Cell{{
							Cpus: []*cmdv1.CPU{
								{Id: 5}, {Id: 6},
								{Id: 7},
							},
						}},
					},
				},
				map[string]string{},
				1),
			Entry("when EmulatorThreadCompleteToEvenParity is enabled and there is one extra CPU assigned for emulatorThread (odd CPUs)",
				v1.CPU{Sockets: 1, Cores: 5, Threads: 1},
				&ConverterContext{CPUSet: []int{5, 6, 7, 8, 9, 10},
					Topology: &cmdv1.Topology{
						NumaCells: []*cmdv1.Cell{{
							Cpus: []*cmdv1.CPU{
								{Id: 5}, {Id: 6}, {Id: 7}, {Id: 8}, {Id: 9},
								{Id: 10},
							},
						}},
					},
				},
				map[string]string{v1.EmulatorThreadCompleteToEvenParity: ""},
				1),
			Entry("when EmulatorThreadCompleteToEvenParity is enabled and there are two extra CPUs assigned for emulatorThread (even CPUs)",
				v1.CPU{Sockets: 1, Cores: 6, Threads: 1},
				&ConverterContext{CPUSet: []int{5, 6, 7, 8, 9, 10, 11, 12},
					Topology: &cmdv1.Topology{
						NumaCells: []*cmdv1.Cell{{
							Cpus: []*cmdv1.CPU{
								{Id: 5}, {Id: 6}, {Id: 7}, {Id: 8}, {Id: 9}, {Id: 10},
								{Id: 11}, {Id: 12},
							},
						}},
					},
				},
				map[string]string{v1.EmulatorThreadCompleteToEvenParity: ""},
				2),
		)
		DescribeTable("should fail assigning CPUs to emulatorThread",
			func(cpu v1.CPU, converterContext *ConverterContext, vmiAnnotations map[string]string, expectedErrorString string) {
				var err error
				domain := &api.Domain{}

				cpuPool := vcpu.NewRelaxedCPUPool(
					&api.CPUTopology{Sockets: cpu.Sockets, Cores: cpu.Cores, Threads: cpu.Threads},
					converterContext.Topology,
					converterContext.CPUSet,
				)
				domain.Spec.CPUTune, err = cpuPool.FitCores()
				Expect(err).ToNot(HaveOccurred())

				vCPUs := hardware.GetNumberOfVCPUs(&cpu)
				_, err = vcpu.FormatEmulatorThreadPin(cpuPool, vmiAnnotations, vCPUs)
				Expect(err).To(MatchError(ContainSubstring(expectedErrorString)))
			},
			Entry("when EmulatorThreadCompleteToEvenParity is disabled and there are not enough CPUs to allocate emulator threads",
				v1.CPU{Sockets: 1, Cores: 2, Threads: 1},
				&ConverterContext{CPUSet: []int{5, 6},
					Topology: &cmdv1.Topology{
						NumaCells: []*cmdv1.Cell{{
							Cpus: []*cmdv1.CPU{
								{Id: 5}, {Id: 6},
							},
						}},
					},
				},
				map[string]string{},
				"no CPU allocated for the emulation thread"),
			Entry("when EmulatorThreadCompleteToEvenParity is enabled and there are not enough Cores to allocate emulator threads (odd CPUs)",
				v1.CPU{Sockets: 1, Cores: 3, Threads: 1},
				&ConverterContext{CPUSet: []int{5, 6, 7},
					Topology: &cmdv1.Topology{
						NumaCells: []*cmdv1.Cell{{
							Cpus: []*cmdv1.CPU{
								{Id: 5}, {Id: 6},
								{Id: 7},
							},
						}},
					},
				},
				map[string]string{v1.EmulatorThreadCompleteToEvenParity: ""},
				"no CPU allocated for the emulation thread"),
			Entry("when EmulatorThreadCompleteToEvenParity is enabled and there are not enough Cores to allocate emulator threads (even CPUs)",
				v1.CPU{Sockets: 1, Cores: 2, Threads: 1},
				&ConverterContext{CPUSet: []int{5, 6, 7},
					Topology: &cmdv1.Topology{
						NumaCells: []*cmdv1.Cell{{
							Cpus: []*cmdv1.CPU{
								{Id: 5}, {Id: 6},
								{Id: 7},
							},
						}},
					},
				},
				map[string]string{v1.EmulatorThreadCompleteToEvenParity: ""},
				"no second CPU allocated for the emulation thread"),
		)
	})

	Context("Correctly handle iothreads with dedicated cpus", func() {
		var vmi *v1.VirtualMachineInstance

		BeforeEach(func() {
			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						CPU: &v1.CPU{DedicatedCPUPlacement: true},
						Resources: v1.ResourceRequirements{
							Requests: k8sv1.ResourceList{
								k8sv1.ResourceMemory: resource.MustParse("64M"),
							},
						},
					},
				},
			}
		})
		It("assigns a set of cpus per iothread, if there are more vcpus than iothreads", func() {
			vmi.Spec.Domain.CPU.Cores = 16
			vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("16")
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			c := &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH),
				CPUSet:         []int{5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
				AllowEmulation: true,
				SMBios:         &cmdv1.SMBios{},
				Topology: &cmdv1.Topology{
					NumaCells: []*cmdv1.Cell{{
						Cpus: []*cmdv1.CPU{
							{Id: 5},
							{Id: 6},
							{Id: 7},
							{Id: 8},
							{Id: 9},
							{Id: 10},
							{Id: 11},
							{Id: 12},
							{Id: 13},
							{Id: 14},
							{Id: 15},
							{Id: 16},
							{Id: 17},
							{Id: 18},
							{Id: 19},
							{Id: 20},
						},
					}},
				},
			}
			domain := vmiToDomain(vmi, c)
			domain.Spec.IOThreads = &api.IOThreads{}
			domain.Spec.IOThreads.IOThreads = uint(6)

			Expect(vcpu.FormatDomainIOThreadPin(vmi, domain, "0", c.CPUSet)).To(Succeed())
			expectedLayout := []api.CPUTuneIOThreadPin{
				{IOThread: 1, CPUSet: "5,6,7"},
				{IOThread: 2, CPUSet: "8,9,10"},
				{IOThread: 3, CPUSet: "11,12,13"},
				{IOThread: 4, CPUSet: "14,15,16"},
				{IOThread: 5, CPUSet: "17,18"},
				{IOThread: 6, CPUSet: "19,20"},
			}
			isExpectedThreadsLayout := equality.Semantic.DeepEqual(expectedLayout, domain.Spec.CPUTune.IOThreadPin)
			Expect(isExpectedThreadsLayout).To(BeTrue())

		})
		It("should pack iothreads equally on available vcpus, if there are more iothreads than vcpus", func() {
			vmi.Spec.Domain.CPU.Cores = 2
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			c := &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				CPUSet:         []int{5, 6},
				AllowEmulation: true,
				Topology: &cmdv1.Topology{
					NumaCells: []*cmdv1.Cell{{
						Cpus: []*cmdv1.CPU{
							{Id: 5},
							{Id: 6},
						},
					}},
				},
			}
			domain := vmiToDomain(vmi, c)
			domain.Spec.IOThreads = &api.IOThreads{}
			domain.Spec.IOThreads.IOThreads = uint(6)

			Expect(vcpu.FormatDomainIOThreadPin(vmi, domain, "0", c.CPUSet)).To(Succeed())
			expectedLayout := []api.CPUTuneIOThreadPin{
				{IOThread: 1, CPUSet: "6"},
				{IOThread: 2, CPUSet: "5"},
				{IOThread: 3, CPUSet: "6"},
				{IOThread: 4, CPUSet: "5"},
				{IOThread: 5, CPUSet: "6"},
				{IOThread: 6, CPUSet: "5"},
			}
			isExpectedThreadsLayout := equality.Semantic.DeepEqual(expectedLayout, domain.Spec.CPUTune.IOThreadPin)
			Expect(isExpectedThreadsLayout).To(BeTrue())
		})
	})
	Context("virtio-net multi-queue", func() {
		var vmi *v1.VirtualMachineInstance

		BeforeEach(func() {
			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "mynamespace",
				},
			}
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Networks = []v1.Network{*v1.DefaultPodNetwork()}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{*v1.DefaultBridgeNetworkInterface()}

			vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue = pointer.P(true)
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{
				k8sv1.ResourceMemory: resource.MustParse("8192Ki"),
				k8sv1.ResourceCPU:    resource.MustParse("2"),
			}
		})

		It("should assign queues to a device if requested", func() {
			var expectedQueues uint = 2
			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores: 2,
			}

			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			Expect(*(domain.Spec.Devices.Interfaces[0].Driver.Queues)).To(Equal(expectedQueues),
				"expected number of queues to equal number of requested vCPUs")
		})
		It("should assign queues to a device if requested based on vcpus", func() {
			var expectedQueues uint = 4

			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores:   2,
				Sockets: 1,
				Threads: 2,
			}
			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			Expect(*(domain.Spec.Devices.Interfaces[0].Driver.Queues)).To(Equal(expectedQueues),
				"expected number of queues to equal number of requested vCPUs")
		})

		It("should not assign queues to a non-virtio devices", func() {
			vmi.Spec.Domain.Devices.Interfaces[0].Model = "e1000"
			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			Expect(domain.Spec.Devices.Interfaces[0].Driver).To(BeNil(),
				"queues should not be set for models other than virtio")
		})

		It("should cap the maximum number of queues", func() {
			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores:   512,
				Sockets: 1,
				Threads: 2,
			}
			domain := vmiToDomain(vmi, &ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH), AllowEmulation: true})
			expectedNumberQueues := uint(multiQueueMaxQueues)
			Expect(*(domain.Spec.Devices.Interfaces[0].Driver.Queues)).To(Equal(expectedNumberQueues),
				"should be capped to the maximum number of queues on tap devices")
		})

	})
	Context("Realtime", func() {
		var vmi *v1.VirtualMachineInstance
		var rtContext *ConverterContext
		BeforeEach(func() {
			rtContext = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				AllowEmulation: true,
				CPUSet:         []int{0, 1, 2, 3, 4},
				Topology: &cmdv1.Topology{
					NumaCells: []*cmdv1.Cell{
						{Id: 0,
							Memory:    &cmdv1.Memory{Amount: 10737418240, Unit: "G"},
							Pages:     []*cmdv1.Pages{{Count: 5, Unit: "G", Size: 1073741824}},
							Distances: []*cmdv1.Sibling{{Id: 0, Value: 1}},
							Cpus:      []*cmdv1.CPU{{Id: 0}, {Id: 1}, {Id: 2}}}}},
			}

			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "mynamespace",
				},
			}
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores:                 2,
				Sockets:               1,
				Threads:               1,
				Realtime:              &v1.Realtime{},
				DedicatedCPUPlacement: true,
			}
		})
		It("should configure the VCPU scheduler information utilizing all pinned vcpus when realtime is enabled", func() {
			domain := vmiToDomain(vmi, rtContext)
			Expect(domain.Spec.Features.PMU).To(Equal(&api.FeatureState{State: "off"}))
		})
	})

	Context("Bootloader", func() {
		var vmi *v1.VirtualMachineInstance
		var c *ConverterContext

		BeforeEach(func() {
			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "mynamespace",
				},
			}

			v1.SetObjectDefaults_VirtualMachineInstance(vmi)

			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				VirtualMachine: vmi,
				AllowEmulation: true,
			}
		})

		Context("when bootloader is not set", func() {
			It("should configure the BIOS bootloader", func() {
				vmi.Spec.Domain.Firmware = &v1.Firmware{}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
				Expect(domainSpec.OS.BootLoader).To(BeNil())
				Expect(domainSpec.OS.NVRam).To(BeNil())
			})
		})

		Context("when bootloader is set", func() {
			It("should configure the BIOS bootloader if no BIOS or EFI option", func() {
				vmi.Spec.Domain.Firmware = &v1.Firmware{
					Bootloader: &v1.Bootloader{},
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
				Expect(domainSpec.OS.BootLoader).To(BeNil())
				Expect(domainSpec.OS.NVRam).To(BeNil())
			})

			It("should configure the BIOS bootloader if BIOS", func() {
				vmi.Spec.Domain.Firmware = &v1.Firmware{
					Bootloader: &v1.Bootloader{
						BIOS: &v1.BIOS{},
					},
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
				Expect(domainSpec.OS.BootLoader).To(BeNil())
				Expect(domainSpec.OS.NVRam).To(BeNil())
			})
		})

		DescribeTable("EFI bootloader", func(secureBoot *bool, efiCode, efiVars string) {
			c.EFIConfiguration = &EFIConfiguration{
				EFICode:      efiCode,
				EFIVars:      efiVars,
				SecureLoader: secureBoot == nil || *secureBoot,
			}

			secureLoader := "yes"
			if secureBoot != nil && !*secureBoot {
				secureLoader = "no"
			}

			vmi.Spec.Domain.Firmware = &v1.Firmware{
				Bootloader: &v1.Bootloader{
					EFI: &v1.EFI{
						SecureBoot: secureBoot,
					},
				},
			}
			vmi.Status.RuntimeUser = 107
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(domainSpec.OS.BootLoader.ReadOnly).To(Equal("yes"))
			Expect(domainSpec.OS.BootLoader.Type).To(Equal("pflash"))
			Expect(domainSpec.OS.BootLoader.Secure).To(Equal(secureLoader))
			Expect(path.Base(domainSpec.OS.BootLoader.Path)).To(Equal(efiCode))
			Expect(path.Base(domainSpec.OS.NVRam.Template)).To(Equal(efiVars))
			Expect(domainSpec.OS.NVRam.NVRam).To(Equal("/var/run/kubevirt-private/libvirt/qemu/nvram/testvmi_VARS.fd"))
		},
			Entry("should use SecureBoot", pointer.P(true), "OVMF_CODE.secboot.fd", "OVMF_VARS.secboot.fd"),
			Entry("should use SecureBoot when SB not defined", nil, "OVMF_CODE.secboot.fd", "OVMF_VARS.secboot.fd"),
			Entry("should not use SecureBoot", pointer.P(false), "OVMF_CODE.fd", "OVMF_VARS.fd"),
			Entry("should not use SecureBoot when OVMF_CODE.fd not present", pointer.P(true), "OVMF_CODE.secboot.fd", "OVMF_VARS.fd"),
		)

		It("EFI vars should be in the right place when running as root", func() {
			c.EFIConfiguration = &EFIConfiguration{
				EFICode:      "OVMF_CODE.fd",
				EFIVars:      "OVMF_VARS.fd",
				SecureLoader: false,
			}

			vmi.Spec.Domain.Firmware = &v1.Firmware{
				Bootloader: &v1.Bootloader{
					EFI: &v1.EFI{
						SecureBoot: pointer.P(false),
					},
				},
			}
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(domainSpec.OS.BootLoader.ReadOnly).To(Equal("yes"))
			Expect(domainSpec.OS.BootLoader.Type).To(Equal("pflash"))
			Expect(domainSpec.OS.BootLoader.Secure).To(Equal("no"))
			Expect(path.Base(domainSpec.OS.BootLoader.Path)).To(Equal(c.EFIConfiguration.EFICode))
			Expect(path.Base(domainSpec.OS.NVRam.Template)).To(Equal(c.EFIConfiguration.EFIVars))
			Expect(domainSpec.OS.NVRam.NVRam).To(Equal("/var/lib/libvirt/qemu/nvram/testvmi_VARS.fd"))
		})

		DescribeTable("display device should be set to", func(arch string, bootloader v1.Bootloader, enableFG bool, expectedDevice string) {
			vmi.Spec.Domain.Firmware = &v1.Firmware{Bootloader: &bootloader}
			c = &ConverterContext{
				Architecture:      archconverter.NewConverter(arch),
				BochsForEFIGuests: enableFG,
				VirtualMachine:    vmi,
				AllowEmulation:    true,
				EFIConfiguration:  &EFIConfiguration{},
			}
			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			Expect(domainSpec.Devices.Video).To(HaveLen(1))
			Expect(domainSpec.Devices.Video[0].Model.Type).To(Equal(expectedDevice))
			if expectedDevice == "bochs" {
				// Bochs doesn't support the vram option
				Expect(domainSpec.Devices.Video[0].Model.VRam).To(BeNil())
			}
		},
			Entry("VGA on amd64 with BIOS and BochsDisplayForEFIGuests unset", amd64, v1.Bootloader{BIOS: &v1.BIOS{}}, false, "vga"),
			Entry("VGA on amd64 with BIOS and BochsDisplayForEFIGuests set", amd64, v1.Bootloader{BIOS: &v1.BIOS{}}, true, "vga"),
			Entry("VGA on amd64 with EFI and BochsDisplayForEFIGuests unset", amd64, v1.Bootloader{EFI: &v1.EFI{}}, false, "vga"),
			Entry("Bochs on amd64 with EFI and BochsDisplayForEFIGuests set", amd64, v1.Bootloader{EFI: &v1.EFI{}}, true, "bochs"),

			Entry("VIRTIO on amd64 with BIOS and BochsDisplayForEFIGuests unset", arm64, v1.Bootloader{BIOS: &v1.BIOS{}}, false, "virtio"),
			Entry("VIRTIO on amd64 with BIOS and BochsDisplayForEFIGuests set", arm64, v1.Bootloader{BIOS: &v1.BIOS{}}, true, "virtio"),
			Entry("VIRTIO on amd64 with EFI and BochsDisplayForEFIGuests unset", arm64, v1.Bootloader{EFI: &v1.EFI{}}, false, "virtio"),
			Entry("VIRTIO on amd64 with EFI and BochsDisplayForEFIGuests set", arm64, v1.Bootloader{EFI: &v1.EFI{}}, true, "virtio"),

			Entry("VIRTIO on s390x with BIOS and BochsDisplayForEFIGuests unset", s390x, v1.Bootloader{BIOS: &v1.BIOS{}}, false, "virtio"),
			Entry("VIRTIO on s390x with BIOS and BochsDisplayForEFIGuests set", s390x, v1.Bootloader{BIOS: &v1.BIOS{}}, true, "virtio"),
			Entry("VIRTIO on s390x with EFI and BochsDisplayForEFIGuests unset", s390x, v1.Bootloader{EFI: &v1.EFI{}}, false, "virtio"),
			Entry("VIRTIO on s390x with EFI and BochsDisplayForEFIGuests set", s390x, v1.Bootloader{EFI: &v1.EFI{}}, true, "virtio"),

			Entry("VGA on ppc64le with BIOS and BochsDisplayForEFIGuests unset", ppc64le, v1.Bootloader{BIOS: &v1.BIOS{}}, false, "vga"),
			Entry("VGA on ppc64le with BIOS and BochsDisplayForEFIGuests set", ppc64le, v1.Bootloader{BIOS: &v1.BIOS{}}, true, "vga"),
			Entry("VGA on ppc64le with EFI and BochsDisplayForEFIGuests unset", ppc64le, v1.Bootloader{EFI: &v1.EFI{}}, false, "vga"),
			Entry("VGA on ppc64le with EFI and BochsDisplayForEFIGuests set", ppc64le, v1.Bootloader{EFI: &v1.EFI{}}, true, "vga"),
		)

		DescribeTable("ACPI table should be set to", func(
			slicVolumeName string, slicVol *v1.Volume,
			msdmVolumeName string, msdmVol *v1.Volume,
			errMatch string,
		) {
			acpi := &v1.ACPI{}
			if slicVolumeName != "" {
				acpi.SlicNameRef = slicVolumeName
				vmi.Spec.Volumes = append(vmi.Spec.Volumes, *slicVol)
			}
			if msdmVolumeName != "" {
				acpi.MsdmNameRef = msdmVolumeName
				vmi.Spec.Volumes = append(vmi.Spec.Volumes, *msdmVol)
			}
			vmi.Spec.Domain.Firmware = &v1.Firmware{ACPI: acpi}

			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				VirtualMachine: vmi,
				AllowEmulation: true,
			}

			if errMatch != "" {
				// The error should be catch by webhook.
				domain := &api.Domain{}
				err := Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, domain, c)
				Expect(err.Error()).To(ContainSubstring(errMatch))
				return
			}

			domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)
			if slicVolumeName != "" {
				Expect(domainSpec.OS.ACPI.Table).To(ContainElement(api.ACPITable{
					Type: "slic",
					Path: filepath.Join(config.GetSecretSourcePath(slicVolumeName), "slic.bin"),
				}))
			}

			if msdmVolumeName != "" {
				Expect(domainSpec.OS.ACPI.Table).To(ContainElement(api.ACPITable{
					Type: "msdm",
					Path: filepath.Join(config.GetSecretSourcePath(msdmVolumeName), "msdm.bin"),
				}))
			}
		},
			// with Valid Secret volumes
			Entry("slic with secret",
				"vol-slic", &v1.Volume{
					Name: "vol-slic",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: "secret-slic",
						},
					},
				}, "", nil, ""),
			Entry("msdm with secret", "", nil,
				"vol-msdm", &v1.Volume{
					Name: "vol-msdm",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: "secret-msdm",
						},
					},
				}, ""),
			// with not valid Volume source
			Entry("slic with configmap",
				"vol-slic", &v1.Volume{
					Name: "vol-slic",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				}, "", nil, "Firmware's volume type is unsupported for slic"),
			Entry("msdm with configmap", "", nil,
				"vol-msdm", &v1.Volume{
					Name: "vol-msdm",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				}, "Firmware's volume type is unsupported for msdm"),
			// without matching volume source
			Entry("slic without volume", "vol-slic", &v1.Volume{}, "", &v1.Volume{}, "Firmware's volume for slic was not found"),
			Entry("msdm without volume", "", &v1.Volume{}, "vol-msdm", &v1.Volume{}, "Firmware's volume for msdm was not found"),
			// try both togeter, correct input
			Entry("slic and msdm with secret",
				"vol-slic", &v1.Volume{
					Name: "vol-slic",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: "secret-slic",
						},
					},
				},
				"vol-msdm", &v1.Volume{
					Name: "vol-msdm",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: "secret-msdm",
						},
					},
				}, ""),
		)
	})

	Context("Kernel Boot", func() {
		var vmi *v1.VirtualMachineInstance
		var c *ConverterContext

		BeforeEach(func() {
			vmi = &v1.VirtualMachineInstance{}

			v1.SetObjectDefaults_VirtualMachineInstance(vmi)

			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				VirtualMachine: vmi,
				AllowEmulation: true,
			}
		})

		Context("when kernel boot is set", func() {
			DescribeTable("should configure the kernel, initrd and Cmdline arguments correctly", func(kernelPath string, initrdPath string, kernelArgs string) {
				vmi.Spec.Domain.Firmware = &v1.Firmware{
					KernelBoot: &v1.KernelBoot{
						KernelArgs: kernelArgs,
						Container: &v1.KernelBootContainer{
							KernelPath: kernelPath,
							InitrdPath: initrdPath,
						},
					},
				}
				domainSpec := vmiToDomainXMLToDomainSpec(vmi, c)

				if kernelPath == "" {
					Expect(domainSpec.OS.Kernel).To(BeEmpty())
				} else {
					Expect(domainSpec.OS.Kernel).To(ContainSubstring(kernelPath))
				}
				if initrdPath == "" {
					Expect(domainSpec.OS.Initrd).To(BeEmpty())
				} else {
					Expect(domainSpec.OS.Initrd).To(ContainSubstring(initrdPath))
				}

				Expect(domainSpec.OS.KernelArgs).To(Equal(kernelArgs))
			},
				Entry("when kernel, initrd and Cmdline are provided", "fully specified path to kernel", "fully specified path to initrd", "some cmdline arguments"),
				Entry("when only kernel and Cmdline are provided", "fully specified path to kernel", "", "some cmdline arguments"),
				Entry("when only kernel and initrd are provided", "fully specified path to kernel", "fully specified path to initrd", ""),
				Entry("when only kernel is provided", "fully specified path to kernel", "", ""),
				Entry("when only initrd and Cmdline are provided", "", "fully specified path to initrd", "some cmdline arguments"),
				Entry("when only Cmdline is provided", "", "", "some cmdline arguments"),
				Entry("when only initrd is provided", "", "fully specified path to initrd", ""),
				Entry("when no arguments provided", "", "", ""),
			)
		})
	})

	Context("hotplug", func() {
		var vmi *v1.VirtualMachineInstance
		var c *ConverterContext

		Context("disk", func() {

			type ConverterFunc = func(name string, disk *api.Disk, c *ConverterContext) error

			BeforeEach(func() {
				vmi = &v1.VirtualMachineInstance{
					ObjectMeta: k8smeta.ObjectMeta{
						Name:      "testvmi",
						Namespace: "mynamespace",
					},
				}

				v1.SetObjectDefaults_VirtualMachineInstance(vmi)

				c = &ConverterContext{
					Architecture:   archconverter.NewConverter(runtime.GOARCH),
					VirtualMachine: vmi,
					AllowEmulation: true,
					IsBlockPVC: map[string]bool{
						"test-block-pvc": true,
					},
					IsBlockDV: map[string]bool{
						"test-block-dv": true,
					},
					VolumesDiscardIgnore: []string{
						"test-discard-ignore",
					},
				}
			})

			DescribeTable("should automatically add virtio-scsi controller", func(arch, expectedModel string) {
				c.Architecture = archconverter.NewConverter(arch)
				domain := vmiToDomain(vmi, c)
				Expect(domain.Spec.Devices.Controllers).To(HaveLen(3))
				foundScsiController := false
				for _, controller := range domain.Spec.Devices.Controllers {
					if controller.Type == "scsi" {
						foundScsiController = true
						Expect(controller.Model).To(Equal(expectedModel))

					}
				}
				Expect(foundScsiController).To(BeTrue(), "did not find SCSI controller when expected")
			},
				Entry("on amd64", amd64, "virtio-non-transitional"),
				Entry("on arm64", arm64, "virtio-non-transitional"),
				Entry("on ppc64le", ppc64le, "virtio-non-transitional"),
				Entry("on s390x", s390x, "virtio-scsi"),
			)

			It("should not automatically add virtio-scsi controller, if hotplug disabled", func() {
				vmi.Spec.Domain.Devices.DisableHotplug = true
				domain := vmiToDomain(vmi, c)
				Expect(domain.Spec.Devices.Controllers).To(HaveLen(2))
			})

			DescribeTable("should convert",
				func(converterFunc ConverterFunc, volumeName string, isBlockMode bool, ignoreDiscard bool) {
					expectedDisk := &api.Disk{}
					expectedDisk.Driver = &api.DiskDriver{}
					expectedDisk.Driver.Type = "raw"
					expectedDisk.Driver.ErrorPolicy = "stop"
					if isBlockMode {
						expectedDisk.Type = "block"
						expectedDisk.Source.Dev = filepath.Join(v1.HotplugDiskDir, volumeName)
					} else {
						expectedDisk.Type = "file"
						expectedDisk.Source.File = fmt.Sprintf("%s.img", filepath.Join(v1.HotplugDiskDir, volumeName))
					}
					if !ignoreDiscard {
						expectedDisk.Driver.Discard = "unmap"
					}

					disk := &api.Disk{
						Driver: &api.DiskDriver{},
					}
					Expect(converterFunc(volumeName, disk, c)).To(Succeed())
					Expect(disk).To(Equal(expectedDisk))
				},
				Entry("filesystem PVC", Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk, "test-fs-pvc", false, false),
				Entry("block mode PVC", Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk, "test-block-pvc", true, false),
				Entry("'discard ignore' PVC", Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk, "test-discard-ignore", false, true),
				Entry("filesystem DV", Convert_v1_Hotplug_DataVolume_To_api_Disk, "test-fs-dv", false, false),
				Entry("block mode DV", Convert_v1_Hotplug_DataVolume_To_api_Disk, "test-block-dv", true, false),
				Entry("'discard ignore' DV", Convert_v1_Hotplug_DataVolume_To_api_Disk, "test-discard-ignore", false, true),
			)
		})

		Context("memory", func() {
			var domain *api.Domain
			var guestMemory resource.Quantity
			var maxGuestMemory resource.Quantity

			BeforeEach(func() {
				guestMemory = resource.MustParse("32Mi")
				maxGuestMemory = resource.MustParse("128Mi")

				vmi = &v1.VirtualMachineInstance{
					ObjectMeta: k8smeta.ObjectMeta{
						Name:      "testvmi",
						Namespace: "mynamespace",
					},
					Spec: v1.VirtualMachineInstanceSpec{
						Domain: v1.DomainSpec{
							Memory: &v1.Memory{
								Guest:    &guestMemory,
								MaxGuest: &maxGuestMemory,
							},
						},
					},
					Status: v1.VirtualMachineInstanceStatus{
						Memory: &v1.MemoryStatus{
							GuestAtBoot:  &guestMemory,
							GuestCurrent: &guestMemory,
						},
					},
				}

				domain = &api.Domain{
					Spec: api.DomainSpec{
						VCPU: &api.VCPU{
							CPUs: 2,
						},
					},
				}

				v1.SetObjectDefaults_VirtualMachineInstance(vmi)

				c = &ConverterContext{
					VirtualMachine: vmi,
					AllowEmulation: true,
				}
			})

			It("should not setup hotplug when maxGuest is missing", func() {
				vmi.Spec.Domain.Memory.MaxGuest = nil
				err := setupDomainMemory(vmi, domain)
				Expect(err).ToNot(HaveOccurred())
				Expect(domain.Spec.MaxMemory).To(BeNil())
			})

			It("should not setup hotplug when maxGuest equals guest memory", func() {
				vmi.Spec.Domain.Memory.MaxGuest = &guestMemory
				err := setupDomainMemory(vmi, domain)
				Expect(err).ToNot(HaveOccurred())
				Expect(domain.Spec.MaxMemory).To(BeNil())
			})

			It("should setup hotplug when maxGuest is set", func() {
				err := setupDomainMemory(vmi, domain)
				Expect(err).ToNot(HaveOccurred())

				Expect(domain.Spec.MaxMemory).ToNot(BeNil())
				Expect(domain.Spec.MaxMemory.Unit).To(Equal("b"))
				Expect(domain.Spec.MaxMemory.Value).To(Equal(uint64(maxGuestMemory.Value())))

				Expect(domain.Spec.Memory).ToNot(BeNil())
				Expect(domain.Spec.Memory.Unit).To(Equal("b"))
				Expect(domain.Spec.Memory.Value).To(Equal(uint64(guestMemory.Value())))
			})
		})
	})

	Context("with AMD SEV LaunchSecurity", func() {
		var (
			vmi *v1.VirtualMachineInstance
			c   *ConverterContext
		)

		BeforeEach(func() {
			vmi = kvapi.NewMinimalVMI("testvmi")
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			vmi.Spec.Domain.Devices.AutoattachMemBalloon = pointer.P(true)
			nonVirtioIface := v1.Interface{Name: "red", Model: "e1000"}
			secondaryNetwork := v1.Network{Name: "red"}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{
				*v1.DefaultBridgeNetworkInterface(), nonVirtioIface,
			}
			vmi.Spec.Networks = []v1.Network{
				*v1.DefaultPodNetwork(), secondaryNetwork,
			}
			vmi.Spec.Domain.LaunchSecurity = &v1.LaunchSecurity{
				SEV: &v1.SEV{},
			}
			vmi.Spec.Domain.Features = &v1.Features{
				SMM: &v1.FeatureState{
					Enabled: pointer.P(false),
				},
			}
			vmi.Spec.Domain.Firmware = &v1.Firmware{
				Bootloader: &v1.Bootloader{
					EFI: &v1.EFI{
						SecureBoot: pointer.P(false),
					},
				},
			}
			c = &ConverterContext{
				Architecture:      archconverter.NewConverter(amd64),
				AllowEmulation:    true,
				EFIConfiguration:  &EFIConfiguration{},
				UseLaunchSecurity: true,
			}
		})

		It("should set LaunchSecurity domain element with 'sev' type and 'NoDebug' policy", func() {
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.LaunchSecurity).ToNot(BeNil())
			Expect(domain.Spec.LaunchSecurity.Type).To(Equal("sev"))
			Expect(domain.Spec.LaunchSecurity.Policy).To(Equal("0x" + strconv.FormatUint(uint64(sev.SEVPolicyNoDebug), 16)))
		})

		It("should set LaunchSecurity domain element with 'sev' type with 'NoDebug' and 'EncryptedState' policy bits", func() {
			// VMI with SEV-ES
			vmi.Spec.Domain.LaunchSecurity = &v1.LaunchSecurity{
				SEV: &v1.SEV{
					Policy: &v1.SEVPolicy{
						EncryptedState: pointer.P(true),
					},
				},
			}
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.LaunchSecurity).ToNot(BeNil())
			Expect(domain.Spec.LaunchSecurity.Type).To(Equal("sev"))
			Expect(domain.Spec.LaunchSecurity.Policy).To(Equal("0x" + strconv.FormatUint(uint64(sev.SEVPolicyNoDebug|sev.SEVPolicyEncryptedState), 16)))
		})

		It("should set IOMMU attribute of the RngDriver", func() {
			rng := &api.Rng{}
			Expect(Convert_v1_Rng_To_api_Rng(&v1.Rng{}, rng, c)).To(Succeed())
			Expect(rng.Driver).ToNot(BeNil())
			Expect(rng.Driver.IOMMU).To(Equal("on"))

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Rng).ToNot(BeNil())
			Expect(domain.Spec.Devices.Rng.Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Rng.Driver.IOMMU).To(Equal("on"))
		})

		It("should set IOMMU attribute of the MemBalloonDriver", func() {
			memBaloon := &api.MemBalloon{}
			ConvertV1ToAPIBalloning(&v1.Devices{}, memBaloon, c)
			Expect(memBaloon.Driver).ToNot(BeNil())
			Expect(memBaloon.Driver.IOMMU).To(Equal("on"))

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning.Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning.Driver.IOMMU).To(Equal("on"))
		})

		It("should set IOMMU attribute of the virtio-net driver", func() {
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(2))
			Expect(domain.Spec.Devices.Interfaces[0].Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces[0].Driver.IOMMU).To(Equal("on"))
			Expect(domain.Spec.Devices.Interfaces[1].Driver).To(BeNil())
		})

		It("should disable the iPXE option ROM", func() {
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(2))
			Expect(domain.Spec.Devices.Interfaces[0].Rom).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces[0].Rom.Enabled).To(Equal("no"))
			Expect(domain.Spec.Devices.Interfaces[1].Rom).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces[1].Rom.Enabled).To(Equal("no"))
		})
	})

	Context("with Secure Execution LaunchSecurity", func() {
		var (
			vmi *v1.VirtualMachineInstance
			c   *ConverterContext
		)

		BeforeEach(func() {
			vmi = kvapi.NewMinimalVMI("testvmi")
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Rng = &v1.Rng{}
			vmi.Spec.Domain.Devices.AutoattachMemBalloon = pointer.P(true)
			virtioIface := v1.Interface{Name: "red", Model: "virtio"}
			secondaryNetwork := v1.Network{Name: "red"}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{
				*v1.DefaultBridgeNetworkInterface(), virtioIface,
			}
			vmi.Spec.Domain.Devices.Disks = []v1.Disk{
				{
					Name: "myvolume",
					DiskDevice: v1.DiskDevice{
						Disk: &v1.DiskTarget{Bus: v1.VirtIO},
					},
				},
			}
			vmi.Spec.Volumes = []v1.Volume{
				{
					Name: "myvolume",
					VolumeSource: v1.VolumeSource{
						HostDisk: &v1.HostDisk{
							Path:     "/var/run/kubevirt-private/vmi-disks/myvolume/disk.img",
							Type:     v1.HostDiskExistsOrCreate,
							Capacity: resource.MustParse("1Gi"),
						},
					},
				},
			}
			vmi.Spec.Networks = []v1.Network{
				*v1.DefaultPodNetwork(), secondaryNetwork,
			}
			vmi.Spec.Domain.LaunchSecurity = &v1.LaunchSecurity{}
			c = &ConverterContext{
				Architecture:      archconverter.NewConverter(s390x),
				AllowEmulation:    true,
				UseLaunchSecurity: true,
			}
		})

		It("should not set LaunchSecurity domain element", func() {
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.LaunchSecurity).To(BeNil())
		})

		It("should set IOMMU attribute of the RngDriver", func() {
			rng := &api.Rng{}
			Expect(Convert_v1_Rng_To_api_Rng(&v1.Rng{}, rng, c)).To(Succeed())
			Expect(rng.Driver).ToNot(BeNil())
			Expect(rng.Driver.IOMMU).To(Equal("on"))

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Rng).ToNot(BeNil())
			Expect(domain.Spec.Devices.Rng.Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Rng.Driver.IOMMU).To(Equal("on"))
		})

		It("should set IOMMU attribute of the MemBalloonDriver", func() {
			memBaloon := &api.MemBalloon{}
			ConvertV1ToAPIBalloning(&v1.Devices{}, memBaloon, c)
			Expect(memBaloon.Driver).ToNot(BeNil())
			Expect(memBaloon.Driver.IOMMU).To(Equal("on"))

			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning.Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning.Driver.IOMMU).To(Equal("on"))
		})

		It("should set IOMMU attribute of the virtio-net driver", func() {
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces).To(HaveLen(2))
			Expect(domain.Spec.Devices.Interfaces[0].Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces[0].Driver.IOMMU).To(Equal("on"))
			Expect(domain.Spec.Devices.Interfaces[1].Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Interfaces[1].Driver.IOMMU).To(Equal("on"))
		})

		It("should set IOMMU attribute of the disk driver", func() {
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())
			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			Expect(domain.Spec.Devices.Disks[0].Driver).ToNot(BeNil())
			Expect(domain.Spec.Devices.Disks[0].Driver.IOMMU).To(Equal("on"))
		})
	})

	Context("when TSC Frequency", func() {
		var (
			vmi *v1.VirtualMachineInstance
			c   *ConverterContext
		)

		const fakeFrequency = 12345

		BeforeEach(func() {
			vmi = kvapi.NewMinimalVMI("testvmi")
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Status.TopologyHints = &v1.TopologyHints{TSCFrequency: pointer.P(int64(fakeFrequency))}
			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				AllowEmulation: true,
			}
		})

		expectTsc := func(domain *api.Domain, expectExists bool) {
			Expect(domain).ToNot(BeNil())
			if !expectExists && domain.Spec.Clock == nil {
				return
			}

			Expect(domain.Spec.Clock).ToNot(BeNil())

			found := false
			for _, timer := range domain.Spec.Clock.Timer {
				if timer.Name == "tsc" {
					actualFrequency, err := strconv.Atoi(timer.Frequency)
					Expect(err).ToNot(HaveOccurred(), "frequency cannot be converted into a number")
					Expect(actualFrequency).To(Equal(fakeFrequency), "set frequency is incorrect")

					found = true
					break
				}
			}

			expectationStr := "exist"
			if !expectExists {
				expectationStr = "not " + expectationStr
			}
			Expect(found).To(Equal(expectExists), fmt.Sprintf("domain TSC frequency is expected to %s", expectationStr))
		}

		Context("is required because VMI is using", func() {
			It("hyperV reenlightenment", func() {
				vmi.Spec.Domain.Features = &v1.Features{
					Hyperv: &v1.FeatureHyperv{
						Reenlightenment: &v1.FeatureState{Enabled: pointer.P(true)},
					},
				}

				domain := vmiToDomain(vmi, c)
				expectTsc(domain, true)
			})

			It("invtsc CPU feature", func() {
				vmi.Spec.Domain.CPU = &v1.CPU{
					Features: []v1.CPUFeature{
						{Name: "invtsc", Policy: "require"},
					},
				}

				domain := vmiToDomain(vmi, c)
				expectTsc(domain, true)
			})
		})

		It("is not required", func() {
			domain := vmiToDomain(vmi, c)
			expectTsc(domain, false)
		})
	})

	Context("with FreePageReporting", func() {
		var (
			vmi *v1.VirtualMachineInstance
			c   *ConverterContext
		)

		BeforeEach(func() {
			vmi = kvapi.NewMinimalVMI("testvmi")
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
		})

		DescribeTable("should set freePageReporting attribute of memballooning device, accordingly to the context value", func(freePageReporting bool, expectedValue string) {
			c = &ConverterContext{
				Architecture:      archconverter.NewConverter(runtime.GOARCH),
				FreePageReporting: freePageReporting,
				AllowEmulation:    true,
			}
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())

			Expect(domain.Spec.Devices).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning).ToNot(BeNil())
			Expect(domain.Spec.Devices.Ballooning.FreePageReporting).To(BeEquivalentTo(expectedValue))
		},
			Entry("when true", true, "on"),
			Entry("when false", false, "off"),
		)
	})

	Context("with Paused strategy", func() {
		var (
			vmi *v1.VirtualMachineInstance
			c   *ConverterContext
		)

		BeforeEach(func() {
			vmi = kvapi.NewMinimalVMI("testvmi")
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
		})

		DescribeTable("bootmenu should be", func(startPaused bool) {
			c = &ConverterContext{
				Architecture:   archconverter.NewConverter(runtime.GOARCH),
				AllowEmulation: true,
			}

			if startPaused {
				vmi.Spec.StartStrategy = pointer.P(v1.StartStrategyPaused)
			}
			domain := vmiToDomain(vmi, c)
			Expect(domain).ToNot(BeNil())

			if startPaused {
				Expect(domain.Spec.OS.BootMenu).ToNot(BeNil())
				Expect(domain.Spec.OS.BootMenu.Enable).To(Equal("yes"))
				Expect(*domain.Spec.OS.BootMenu.Timeout).To(Equal(bootMenuTimeoutMS))
			} else {
				Expect(domain.Spec.OS.BootMenu).To(BeNil())
			}

		},
			Entry("enabled when set", true),
			Entry("disabled when not set", false),
		)
	})

	Context("TPM", func() {
		DescribeTable("should", func(vmiTPM *v1.TPMDevice, matcher types.GomegaMatcher) {
			vmi := libvmi.New()
			vmi.Spec.Domain.Devices.TPM = vmiTPM
			domain := vmiToDomain(
				vmi,
				&ConverterContext{
					Architecture:   archconverter.NewConverter(runtime.GOARCH),
					AllowEmulation: true,
				},
			)
			Expect(domain.Spec.Devices.TPMs).To(matcher)
		},
			Entry("be enabled within domain when empty device provided in VMI",
				&v1.TPMDevice{},
				ContainElement(api.TPM{
					Model: "tpm-tis",
					Backend: api.TPMBackend{
						Type:    "emulator",
						Version: "2.0",
					},
				}),
			),
			Entry("be enabled within domain when device provided and explicitly enabled in VMI",
				&v1.TPMDevice{Enabled: pointer.P(true)},
				ContainElement(api.TPM{
					Model: "tpm-tis",
					Backend: api.TPMBackend{
						Type:    "emulator",
						Version: "2.0",
					},
				}),
			),
			Entry("be enabled within domain when device provided in VMI with persistent=true",
				&v1.TPMDevice{Persistent: pointer.P(true)},
				ContainElement(api.TPM{
					Model: "tpm-crb",
					Backend: api.TPMBackend{
						Type:            "emulator",
						Version:         "2.0",
						PersistentState: "yes",
					},
				}),
			),
			Entry("not be present within domain when nil in VMI",
				nil,
				BeEmpty(),
			),
			Entry("not be present within domain when explicitly disabled in VMI",
				&v1.TPMDevice{Enabled: pointer.P(false), Persistent: pointer.P(true)},
				BeEmpty(),
			),
		)
	})
})

var _ = Describe("disk device naming", func() {
	It("format device name should return correct value", func() {
		res := FormatDeviceName("sd", 0)
		Expect(res).To(Equal("sda"))
		res = FormatDeviceName("sd", 1)
		Expect(res).To(Equal("sdb"))
		// 25 is z 26 starting at 0
		res = FormatDeviceName("sd", 25)
		Expect(res).To(Equal("sdz"))
		res = FormatDeviceName("sd", 26*2-1)
		Expect(res).To(Equal("sdaz"))
		res = FormatDeviceName("sd", 26*26-1)
		Expect(res).To(Equal("sdyz"))
	})

	It("makeDeviceName should generate proper name", func() {
		prefixMap := make(map[string]deviceNamer)
		res, index := makeDeviceName("test1", v1.VirtIO, prefixMap)
		Expect(res).To(Equal("vda"))
		Expect(index).To(Equal(0))
		for i := 2; i < 10; i++ {
			makeDeviceName(fmt.Sprintf("test%d", i), v1.VirtIO, prefixMap)
		}
		prefix := getPrefixFromBus(v1.VirtIO)
		delete(prefixMap[prefix].usedDeviceMap, "vdd")
		By("Verifying next value is vdd")
		res, index = makeDeviceName("something", v1.VirtIO, prefixMap)
		Expect(index).To(Equal(3))
		Expect(res).To(Equal("vdd"))
		res, index = makeDeviceName("something_else", v1.VirtIO, prefixMap)
		Expect(res).To(Equal("vdj"))
		Expect(index).To(Equal(9))
		By("verifying existing returns correct value")
		res, index = makeDeviceName("something", v1.VirtIO, prefixMap)
		Expect(res).To(Equal("vdd"))
		Expect(index).To(Equal(3))
		By("Verifying a new bus returns from start")
		res, index = makeDeviceName("something", "scsi", prefixMap)
		Expect(res).To(Equal("sda"))
		Expect(index).To(Equal(0))
	})
})

var _ = Describe("direct IO checker", func() {
	var directIOChecker DirectIOChecker
	var tmpDir string
	var existingFile string
	var nonExistingFile string
	var err error

	BeforeEach(func() {
		directIOChecker = NewDirectIOChecker()
		tmpDir, err = os.MkdirTemp("", "direct-io-checker")
		Expect(err).ToNot(HaveOccurred())
		existingFile = filepath.Join(tmpDir, "disk.img")
		Expect(os.WriteFile(existingFile, []byte("test"), 0644)).To(Succeed())
		nonExistingFile = filepath.Join(tmpDir, "non-existing-file")
	})

	AfterEach(func() {
		Expect(os.RemoveAll(tmpDir)).To(Succeed())
	})

	It("should not fail when file/device exists", func() {
		_, err = directIOChecker.CheckFile(existingFile)
		Expect(err).ToNot(HaveOccurred())
		_, err = directIOChecker.CheckBlockDevice(existingFile)
		Expect(err).ToNot(HaveOccurred())
	})

	It("should not fail when file does not exist", func() {
		_, err := directIOChecker.CheckFile(nonExistingFile)
		Expect(err).ToNot(HaveOccurred())
		_, err = os.Stat(nonExistingFile)
		Expect(err).To(MatchError(fs.ErrNotExist))
	})

	It("should fail when device does not exist", func() {
		_, err := directIOChecker.CheckBlockDevice(nonExistingFile)
		Expect(err).To(HaveOccurred())
		_, err = os.Stat(nonExistingFile)
		Expect(err).To(MatchError(fs.ErrNotExist))
	})

	It("should fail when the path does not exist", func() {
		nonExistingPath := "/non/existing/path/disk.img"
		_, err = directIOChecker.CheckFile(nonExistingPath)
		Expect(err).To(MatchError(fs.ErrNotExist))
		_, err = directIOChecker.CheckBlockDevice(nonExistingPath)
		Expect(err).To(MatchError(fs.ErrNotExist))
		_, err = os.Stat(nonExistingPath)
		Expect(err).To(MatchError(fs.ErrNotExist))
	})
})

var _ = Describe("SetDriverCacheMode", func() {
	var ctrl *gomock.Controller
	var mockDirectIOChecker *MockDirectIOChecker

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		mockDirectIOChecker = NewMockDirectIOChecker(ctrl)
	})

	expectCheckTrue := func() {
		mockDirectIOChecker.EXPECT().CheckBlockDevice(gomock.Any()).AnyTimes().Return(true, nil)
		mockDirectIOChecker.EXPECT().CheckFile(gomock.Any()).AnyTimes().Return(true, nil)
	}

	expectCheckFalse := func() {
		mockDirectIOChecker.EXPECT().CheckBlockDevice(gomock.Any()).AnyTimes().Return(false, nil)
		mockDirectIOChecker.EXPECT().CheckFile(gomock.Any()).AnyTimes().Return(false, nil)
	}

	expectCheckError := func() {
		checkerError := fmt.Errorf("DirectIOChecker error")
		mockDirectIOChecker.EXPECT().CheckBlockDevice(gomock.Any()).AnyTimes().Return(false, checkerError)
		mockDirectIOChecker.EXPECT().CheckFile(gomock.Any()).AnyTimes().Return(false, checkerError)
	}

	DescribeTable("should correctly set driver cache mode", func(cache, expectedCache string, setExpectations func()) {
		disk := &api.Disk{
			Driver: &api.DiskDriver{
				Cache: cache,
			},
			Source: api.DiskSource{
				File: "file",
			},
		}
		setExpectations()
		err := SetDriverCacheMode(disk, mockDirectIOChecker)
		if expectedCache == "" {
			Expect(err).To(HaveOccurred())
		} else {
			Expect(err).ToNot(HaveOccurred())
			Expect(disk.Driver.Cache).To(Equal(expectedCache))
		}
	},
		Entry("detect 'none' with direct io", "", string(v1.CacheNone), expectCheckTrue),
		Entry("detect 'writethrough' without direct io", "", string(v1.CacheWriteThrough), expectCheckFalse),
		Entry("fallback to 'writethrough' on error", "", string(v1.CacheWriteThrough), expectCheckError),
		Entry("keep 'none' with direct io", string(v1.CacheNone), string(v1.CacheNone), expectCheckTrue),
		Entry("return error without direct io", string(v1.CacheNone), "", expectCheckFalse),
		Entry("return error on error", string(v1.CacheNone), "", expectCheckError),
		Entry("'writethrough' with direct io", string(v1.CacheWriteThrough), string(v1.CacheWriteThrough), expectCheckTrue),
		Entry("'writethrough' without direct io", string(v1.CacheWriteThrough), string(v1.CacheWriteThrough), expectCheckFalse),
		Entry("'writethrough' on error", string(v1.CacheWriteThrough), string(v1.CacheWriteThrough), expectCheckError),
	)
})

func diskToDiskXML(arch string, disk *v1.Disk) string {
	devicePerBus := make(map[string]deviceNamer)
	libvirtDisk := &api.Disk{}
	Expect(Convert_v1_Disk_To_api_Disk(&ConverterContext{Architecture: archconverter.NewConverter(arch), UseVirtioTransitional: false}, disk, libvirtDisk, devicePerBus, nil, make(map[string]v1.VolumeStatus))).To(Succeed())
	data, err := xml.MarshalIndent(libvirtDisk, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	return string(data)
}

func vmiToDomainXML(vmi *v1.VirtualMachineInstance, c *ConverterContext) string {
	domain := vmiToDomain(vmi, c)
	data, err := xml.MarshalIndent(domain.Spec, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	return string(data)
}

func vmiToDomain(vmi *v1.VirtualMachineInstance, c *ConverterContext) *api.Domain {
	domain := &api.Domain{}
	ExpectWithOffset(1, Convert_v1_VirtualMachineInstance_To_api_Domain(vmi, domain, c)).To(Succeed())
	api.NewDefaulter(c.Architecture.GetArchitecture()).SetObjectDefaults_Domain(domain)
	return domain
}

func xmlToDomainSpec(data string) *api.DomainSpec {
	newDomain := &api.DomainSpec{}
	ExpectWithOffset(1, xml.Unmarshal([]byte(data), newDomain)).To(Succeed())
	newDomain.XMLName.Local = ""
	newDomain.XmlNS = "http://libvirt.org/schemas/domain/qemu/1.0"
	return newDomain
}

func vmiToDomainXMLToDomainSpec(vmi *v1.VirtualMachineInstance, c *ConverterContext) *api.DomainSpec {
	return xmlToDomainSpec(vmiToDomainXML(vmi, c))
}

// As the arch specific default disk is set in the mutating webhook, so in some tests,
// it needs to run the mutate function before verifying converter
func vmiArchMutate(arch string, vmi *v1.VirtualMachineInstance, c *ConverterContext) {
	switch arch {
	case arm64:
		defaults.SetArm64Defaults(&vmi.Spec)
		// bootloader has been initialized in webhooks.SetArm64Defaults,
		// c.EFIConfiguration.SecureLoader is needed in the converter.Convert_v1_VirtualMachineInstance_To_api_Domain.
		c.EFIConfiguration = &EFIConfiguration{
			SecureLoader: false,
		}
	case amd64, ppc64le:
		defaults.SetAmd64Defaults(&vmi.Spec)
	case s390x:
		defaults.SetS390xDefaults(&vmi.Spec)
	}
}

var _ = Describe("Defaults", func() {
	It("should set the default watchdog and the default watchdog action for amd64", func() {
		vmi := &v1.VirtualMachineInstance{
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						Watchdog: &v1.Watchdog{
							WatchdogDevice: v1.WatchdogDevice{
								I6300ESB: &v1.I6300ESBWatchdog{},
							},
						},
					},
				},
			},
		}

		defaults.SetAmd64Watchdog(&vmi.Spec)
		Expect(vmi.Spec.Domain.Devices.Watchdog.I6300ESB.Action).To(Equal(v1.WatchdogActionReset))

		vmi.Spec.Domain.Devices.Watchdog.I6300ESB = nil
		defaults.SetAmd64Watchdog(&vmi.Spec)
		Expect(vmi.Spec.Domain.Devices.Watchdog.I6300ESB).ToNot(BeNil())
		Expect(vmi.Spec.Domain.Devices.Watchdog.I6300ESB.Action).To(Equal(v1.WatchdogActionReset))
	})

	It("should not set a watchdog if none is defined on amd64", func() {
		vmi := &v1.VirtualMachineInstance{
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{},
				},
			},
		}

		defaults.SetAmd64Watchdog(&vmi.Spec)
		Expect(vmi.Spec.Domain.Devices.Watchdog).To(BeNil())
	})

	It("should set the default watchdog and the default watchdog action for s390x", func() {
		vmi := &v1.VirtualMachineInstance{
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						Watchdog: &v1.Watchdog{
							WatchdogDevice: v1.WatchdogDevice{
								Diag288: &v1.Diag288Watchdog{},
							},
						},
					},
				},
			},
		}

		defaults.SetS390xWatchdog(&vmi.Spec)
		Expect(vmi.Spec.Domain.Devices.Watchdog.Diag288.Action).To(Equal(v1.WatchdogActionReset))

		vmi.Spec.Domain.Devices.Watchdog.Diag288 = nil
		defaults.SetS390xWatchdog(&vmi.Spec)
		Expect(vmi.Spec.Domain.Devices.Watchdog.Diag288).ToNot(BeNil())
		Expect(vmi.Spec.Domain.Devices.Watchdog.Diag288.Action).To(Equal(v1.WatchdogActionReset))
	})

	It("should not set a watchdog if none is defined on s390x", func() {
		vmi := &v1.VirtualMachineInstance{
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{},
				},
			},
		}

		defaults.SetS390xWatchdog(&vmi.Spec)
		Expect(vmi.Spec.Domain.Devices.Watchdog).To(BeNil())
	})
})
