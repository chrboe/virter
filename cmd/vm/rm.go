package vm

import (
	"log"

	"github.com/spf13/cobra"

	connect "github.com/LINBIT/virter/cmd"
)

// rmCmd represents the rm command
var rmCmd = &cobra.Command{
	Use:   "rm",
	Short: "Start a virtual machine",
	Long:  `Start a fresh virtual machine from an image.`,
	Run:   vmRm,
}

func init() {
	vmCmd.AddCommand(rmCmd)

	rmCmd.Flags().StringVarP(&vmName, "name", "n", "", "name of the VM to remove")
	rmCmd.MarkFlagRequired("name")
}

func vmRm(cmd *cobra.Command, args []string) {
	v, err := connect.VirterConnect()
	if err != nil {
		log.Fatal(err)
	}

	err = v.VMRm(vmName)
	if err != nil {
		log.Fatal(err)
	}
}