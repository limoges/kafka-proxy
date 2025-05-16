package proxy

import (
	"net"

	"github.com/pkg/errors"
)

type BrokerMap struct {
	listenerConfigs map[string]*ListenerConfig
}

func NewBrokerMap() *BrokerMap {
	m := &BrokerMap{}
	m.listenerConfigs = make(map[string]*ListenerConfig)
	return m
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
