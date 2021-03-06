package cmd

import (
	"fmt"
	"net"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/spf13/viper"

	"github.com/LINBIT/virter/internal/virter"
)

// VirterConnect connects to a local libvirt instance
func VirterConnect() (*virter.Virter, error) {
	c, err := net.DialTimeout("unix", "/var/run/libvirt/libvirt-sock", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to dial libvirt: %w", err)
	}

	l := libvirt.New(c)
	if err := l.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to libvirt socket: %w", err)
	}

	pool := viper.GetString("libvirt.pool")
	network := viper.GetString("libvirt.network")

	return virter.New(l, pool, network), nil
}
