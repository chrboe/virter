package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/rck/unit"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/LINBIT/virter/internal/virter"
	"github.com/LINBIT/virter/pkg/registry"
)

var sizeUnits = func() map[string]int64 {
	units := unit.DefaultUnits
	units["KiB"] = units["K"]
	units["MiB"] = units["M"]
	units["GiB"] = units["G"]
	units["TiB"] = units["T"]
	units["PiB"] = units["P"]
	units["EiB"] = units["E"]
	return units
}()

func createConsoleDir(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	// libvirt doesn't like relative paths
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("failed to determine absolute path for console directory '%v': %w",
			path, err)
	}

	if err := os.MkdirAll(absPath, 0700); err != nil {
		return "", fmt.Errorf("failed to create console directory at '%v': %w", absPath, err)
	}

	return absPath, nil
}

func createConsoleFile(consoleDir, vmName string) (string, error) {
	if consoleDir == "" {
		return "", nil
	}

	consolePath := filepath.Join(consoleDir, vmName+".log")

	file, err := os.Create(consolePath)
	if err != nil {
		return "", fmt.Errorf("failed to create console file at '%v': %w", consolePath, err)
	}
	file.Close()

	return consolePath, nil
}

func pullIfNotExists(v *virter.Virter, imageName string) error {
	exists, err := v.ImageExists(imageName)
	if err != nil {
		return fmt.Errorf("could not determine whether or not image %v exists: %w",
			imageName, err)
	}
	if !exists {
		log.Printf("Image %v not available locally, pulling", imageName)
		e := pullImage(v, imageName, "")
		if errors.Is(e, registry.ErrNotFound) {
			return fmt.Errorf("Could not find image %v", imageName)
		} else if e != nil {
			return fmt.Errorf("Error pulling image %v: %w", imageName, e)
		}
	}

	return nil
}

func vmRunCommand() *cobra.Command {
	var vmName string
	var vmID uint
	var count uint
	var waitSSH bool

	var mem *unit.Value
	var memKiB uint64

	var bootCapacity *unit.Value
	var bootCapacityKiB uint64

	var vcpus uint

	var consoleDir string

	var diskStrings []string
	var disks []virter.Disk

	var provisionFile string
	var provisionOverrides []string

	runCmd := &cobra.Command{
		Use:   "run image",
		Short: "Start a virtual machine with a given image",
		Long:  `Start a fresh virtual machine from an image.`,
		Args:  cobra.ExactArgs(1),
		PreRun: func(cmd *cobra.Command, args []string) {
			memKiB = uint64(mem.Value / unit.DefaultUnits["K"])
			bootCapacityKiB = uint64(bootCapacity.Value / unit.DefaultUnits["K"])

			for _, s := range diskStrings {
				var d DiskArg
				err := d.Set(s)
				if err != nil {
					log.Fatalf("Invalid disk: %v", err)
				}
				disks = append(disks, &d)
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			v, err := VirterConnect()
			if err != nil {
				log.Fatal(err)
			}
			defer v.ForceDisconnect()

			imageName := args[0]

			publicKeys, err := loadPublicKeys()
			if err != nil {
				log.Fatal(err)
			}

			privateKey, err := loadPrivateKey()
			if err != nil {
				log.Fatal(err)
			}

			consoleDir, err = createConsoleDir(consoleDir)
			if err != nil {
				log.Fatalf("Error while creating console directory: %v", err)
			}

			err = pullIfNotExists(v, imageName)
			if err != nil {
				log.Fatal(err)
			}

			// do we want to run provisioning steps?
			provision := provisionFile != "" || len(provisionOverrides) > 0

			// if we want to run some provisioning steps later,
			// it doesn't make sense not to wait for SSH.
			if provision {
				waitSSH = true
			}

			var g errgroup.Group
			var i uint

			// save the VM names in case we want to provision later
			vmNames := make([]string, count)
			for i = 0; i < count; i++ {
				i := i
				id := vmID + i
				g.Go(func() error {
					var thisVMName string
					if vmName == "" {
						// if the name is not set, use image name + id
						thisVMName = fmt.Sprintf("%s-%d", imageName, id)
					} else if !cmd.Flags().Changed("count") {
						// if it is set, use the supplied name if
						// --count is the default (1)
						thisVMName = vmName
					} else {
						// if the count is set explicitly, use the
						// supplied name and the id
						thisVMName = fmt.Sprintf("%s-%d", vmName, id)
					}
					vmNames[i] = thisVMName

					consolePath, err := createConsoleFile(consoleDir, thisVMName)
					if err != nil {
						log.Fatalf("Error while creating console file: %v", err)
					}

					c := virter.VMConfig{
						ImageName:       imageName,
						Name:            thisVMName,
						MemoryKiB:       memKiB,
						BootCapacityKiB: bootCapacityKiB,
						VCPUs:           vcpus,
						ID:              id,
						SSHPublicKeys:   publicKeys,
						SSHPrivateKey:   privateKey,
						WaitSSH:         waitSSH,
						SSHPingCount:    viper.GetInt("time.ssh_ping_count"),
						SSHPingPeriod:   viper.GetDuration("time.ssh_ping_period"),
						ConsolePath:     consolePath,
						Disks:           disks,
					}

					err = v.VMRun(SSHClientBuilder{}, c)
					if err != nil {
						return fmt.Errorf("Failed to start VM %d: %w", id, err)
					}
					return nil
				})
			}
			if err := g.Wait(); err != nil {
				log.Fatal(err)
			}

			if provision {
				provOpt := virter.ProvisionOption{
					FilePath:  provisionFile,
					Overrides: provisionOverrides,
				}
				if err := execProvision(provOpt, vmNames); err != nil {
					log.Fatal(err)
				}
			}
		},
	}

	runCmd.Flags().StringVarP(&vmName, "name", "n", "", "name of new VM")
	runCmd.Flags().UintVarP(&vmID, "id", "", 0, "ID for VM which determines the IP address")
	runCmd.MarkFlagRequired("id")
	runCmd.Flags().UintVar(&count, "count", 1, "Number of VMs to start")
	runCmd.Flags().BoolVarP(&waitSSH, "wait-ssh", "w", false, "whether to wait for SSH port (default false)")
	u := unit.MustNewUnit(sizeUnits)
	mem = u.MustNewValue(1*sizeUnits["G"], unit.None)
	runCmd.Flags().VarP(mem, "memory", "m", "Set amount of memory for the VM")
	bootCapacity = u.MustNewValue(0, unit.None)
	runCmd.Flags().VarP(bootCapacity, "bootcapacity", "", "Capacity of the boot volume (default is the capacity of the base image, at least 10G)")
	runCmd.Flags().UintVar(&vcpus, "vcpus", 1, "Number of virtual CPUs to allocate for the VM")
	runCmd.Flags().StringVarP(&consoleDir, "console", "c", "", "Directory to save the VMs console outputs to")

	// Unfortunately, pflag cannot accept arrays of custom Values (yet?).
	// See https://github.com/spf13/pflag/issues/260
	// For us, this means that we have to read the disks as strings first,
	// and then manually marshal them to Disks.
	// If this ever gets implemented in pflag , we will be able to solve this
	// in a much smoother way.
	runCmd.Flags().StringArrayVarP(&diskStrings, "disk", "d", []string{}, `Add a disk to the VM. Format: "name=disk1,size=100MiB,format=qcow2,bus=virtio". Can be specified multiple times`)
	runCmd.Flags().StringVarP(&provisionFile, "provision", "p", "", "name of toml file containing provisioning steps")
	runCmd.Flags().StringSliceVarP(&provisionOverrides, "set", "s", []string{}, "set/override provisioning steps")

	return runCmd
}
