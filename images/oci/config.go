package oci

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

// NamespaceName is this backend's meta namespace.
const NamespaceName = "images_oci"

type Config struct {
	images.BaseConfig
	// PoolSize bounds concurrent layer processing.
	PoolSize int
}

func NewConfig(rootDir string, poolSize int) *Config {
	return &Config{
		RootDir: rootDir, Subdir: "oci", BlobExt: ".erofs", Name: NamespaceName,
		PoolSize: utils.PoolSizeOrDefault(poolSize),
	}
}

func (c *Config) EnsureDirs() error {
	if err := c.BaseConfig.EnsureDirs(); err != nil {
		return err
	}
	return utils.EnsureDirs(c.BootBaseDir())
}

func (c *Config) BootBaseDir() string { return filepath.Join(c.BackendDir(), "boot") }

func (c *Config) BootDir(layerDigestHex string) string {
	return filepath.Join(c.BootBaseDir(), layerDigestHex)
}

func (c *Config) KernelPath(layerDigestHex string) string {
	return filepath.Join(c.BootDir(layerDigestHex), "vmlinuz")
}

func (c *Config) InitrdPath(layerDigestHex string) string {
	return filepath.Join(c.BootDir(layerDigestHex), "initrd.img")
}
