package nvml

import (
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/sirupsen/logrus"
)

type Client struct {
	nvml.Interface
	log *logrus.Logger
}

func NewClient(libraryPath string, log *logrus.Logger) *Client {
	var opts []nvml.LibraryOption
	if libraryPath != "" {
		opts = append(opts, nvml.WithLibraryPath(libraryPath))
	}

	nvmllib := nvml.New(opts...)

	return &Client{
		log:       log,
		Interface: nvmllib,
	}
}

func (n Client) ValidateDriver() error {
	if ret := n.Init(); ret != nvml.SUCCESS {
		n.log.Infof("Failed to initialize NVML : %v", ret)
		return ret
	}
	defer func() {
		_ = n.Shutdown()
	}()

	version, ret := n.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		n.log.Infof("NVML library returned an error: %v", ret)
		return ret
	}

	n.log.Infof("Host driver detected: %s", version)
	return nil
}
