package proxy

import (
	"fmt"
	"net"

	"github.com/grepplabs/kafka-proxy/config"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// BrokerMap maintains a map of usptream brokers, associated listeners and avertised addresses.
type BrokerMap struct {
	listenerConfigs map[string]*ListenerConfig
}

type BrokerMapOption func(*BrokerMap) error

func NewBrokerMap(opts ...BrokerMapOption) (*BrokerMap, error) {
	m := &BrokerMap{}
	m.listenerConfigs = make(map[string]*ListenerConfig)

	for _, opt := range opts {
		if err := opt(m); err != nil {
			return m, err
		}
	}
	return m, nil
}

func BrokerMapConfig(cfg *config.Config) BrokerMapOption {
	return func(brokerMap *BrokerMap) error {
		for _, v := range cfg.Proxy.BootstrapServers {
			if lc, ok := brokerMap.GetListenerConfig(v.BrokerAddress); ok {
				if lc.ListenerAddress != v.ListenerAddress || lc.AdvertisedAddress != v.AdvertisedAddress {
					return fmt.Errorf("bootstrap server mapping %s configured twice: %v and %v", v.BrokerAddress, v, lc.ToListenerConfig())
				}
				continue
			}
			logrus.Infof("Bootstrap server %s advertised as %s", v.BrokerAddress, v.AdvertisedAddress)
			brokerMap.SetListenerConfig(v.BrokerAddress, FromListenerConfig(v))
		}

		externalToListenerConfig := make(map[string]config.ListenerConfig)
		for _, v := range cfg.Proxy.ExternalServers {
			if lc, ok := externalToListenerConfig[v.BrokerAddress]; ok {
				if lc.ListenerAddress != v.ListenerAddress {
					return fmt.Errorf("external server mapping %s configured twice: %s and %v", v.BrokerAddress, v.ListenerAddress, lc)
				}
				continue
			}
			if v.ListenerAddress != v.AdvertisedAddress {
				return fmt.Errorf("external server mapping has different listener and advertised addresses %v", v)
			}
			externalToListenerConfig[v.BrokerAddress] = v
		}

		for _, v := range externalToListenerConfig {
			if lc, ok := brokerMap.GetListenerConfig(v.BrokerAddress); ok {
				if lc.AdvertisedAddress != v.AdvertisedAddress {
					return fmt.Errorf("bootstrap and external server mappings %s with different advertised addresses: %v and %v", v.BrokerAddress, v.ListenerAddress, lc.AdvertisedAddress)
				}
				continue
			}
			logrus.Infof("External server %s advertised as %s", v.BrokerAddress, v.AdvertisedAddress)
			brokerMap.SetListenerConfig(v.BrokerAddress, FromListenerConfig(v))
		}
		return nil
	}
}

func (m *BrokerMap) GetBrokerByAdvertisedHost(host string) (brokerAddress string, brokerId int32, err error) {
	for _, c := range m.listenerConfigs {
		advertisedHost, _, _ := net.SplitHostPort(c.AdvertisedAddress)
		if advertisedHost == host {
			return c.GetBrokerAddress(), c.BrokerID, nil
		}
	}
	return "", 0, errors.New("not found")
}

func (m *BrokerMap) FindListenerConfig(brokerId int32) *ListenerConfig {
	for _, listenerConfig := range m.listenerConfigs {
		if listenerConfig.BrokerID == brokerId {
			return listenerConfig
		}
	}
	return nil
}

func (m *BrokerMap) DeleteListenerConfig(brokerAddress string) {
	delete(m.listenerConfigs, brokerAddress)
}

func (m *BrokerMap) GetListenerConfig(brokerAddress string) (*ListenerConfig, bool) {
	l, ok := m.listenerConfigs[brokerAddress]
	return l, ok
}
func (m *BrokerMap) SetListenerConfig(brokerAddress string, cfg *ListenerConfig) {
	m.listenerConfigs[brokerAddress] = cfg
}
