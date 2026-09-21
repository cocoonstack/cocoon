package firecracker

import (
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
)

type Config struct {
	hypervisor.BaseConfig
}

func NewConfig(conf *config.Config) *Config {
	return &Config{BaseConfig: hypervisor.NewBaseConfig(conf, "firecracker", conf.FCBinary, pidFileName)}
}
