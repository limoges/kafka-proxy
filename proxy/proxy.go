package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"sync"
	"text/template"

	"github.com/grepplabs/kafka-proxy/config"
	"github.com/grepplabs/kafka-proxy/pkg/libs/util"
	"github.com/pires/go-proxyproto"
	pp "github.com/pires/go-proxyproto"
	"github.com/sirupsen/logrus"
)

type ListenFunc func(listenAddress string) (l net.Listener, err error)

type Listeners struct {
	// Source of new connections to Kafka broker.
	connSrc chan Conn
	// listen IP for dynamically start
	defaultListenerIP string
	// advertised address for dynamic listeners
	dynamicAdvertisedListener string
	// socket TCP options
	tcpConnOptions TCPConnOptions

	listenFunc ListenFunc
	tlsConfig  *tls.Config

	deterministicListeners   bool
	disableDynamicListeners  bool
	dynamicSequentialMinPort int

	brokerToListenerConfig map[string]*ListenerConfig
	lock                   sync.RWMutex
	useMultiHostListener   bool
	usePPv2                bool
}

func NewListeners(cfg *config.Config) (*Listeners, error) {
	tcpConnOptions := TCPConnOptions{
		KeepAlive:       cfg.Proxy.ListenerKeepAlive,
		ReadBufferSize:  cfg.Proxy.ListenerReadBufferSize,
		WriteBufferSize: cfg.Proxy.ListenerWriteBufferSize,
	}

	var tlsConfig *tls.Config
	if cfg.Proxy.TLS.Enable {
		var err error
		tlsConfig, err = newTLSListenerConfig(cfg)
		if err != nil {
			return nil, err
		}
	}

	listenFunc := func(listenAddress string) (ln net.Listener, err error) {

		if tlsConfig != nil {
			ln, err = tls.Listen("tcp", listenAddress, tlsConfig)
			if err != nil {
				return ln, err
			}
		} else {
			ln, err = net.Listen("tcp", listenAddress)
			if err != nil {
				return ln, err
			}
		}

		// We wrap a tcp or tls listener with proxy protocol v2.
		// The data on the connection will look like [ppv2 headers][tls headers] tcp data.
		if cfg.Proxy.ProxyProtocolV2.Enable {
			fmt.Println("proxy/proxy.go: wrapping tcp listener with proxy protocol v2")
			ln = &pp.Listener{Listener: ln}
		}
		return ln, nil
	}

	brokerToListenerConfig, err := getBrokerToListenerConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &Listeners{
		defaultListenerIP:         cfg.Proxy.DefaultListenerIP,
		dynamicAdvertisedListener: cfg.Proxy.DynamicAdvertisedListener,
		connSrc:                   make(chan Conn, 1),
		brokerToListenerConfig:    brokerToListenerConfig,
		tlsConfig:                 tlsConfig,
		tcpConnOptions:            tcpConnOptions,
		listenFunc:                listenFunc,
		deterministicListeners:    cfg.Proxy.DeterministicListeners,
		disableDynamicListeners:   cfg.Proxy.DisableDynamicListeners,
		dynamicSequentialMinPort:  cfg.Proxy.DynamicSequentialMinPort,
		useMultiHostListener:      true,
		usePPv2:                   cfg.Proxy.ProxyProtocolV2.Enable,
	}, nil
}

func getBrokerToListenerConfig(cfg *config.Config) (map[string]*ListenerConfig, error) {
	brokerToListenerConfig := make(map[string]*ListenerConfig)

	for _, v := range cfg.Proxy.BootstrapServers {
		if lc, ok := brokerToListenerConfig[v.BrokerAddress]; ok {
			if lc.ListenerAddress != v.ListenerAddress || lc.AdvertisedAddress != v.AdvertisedAddress {
				return nil, fmt.Errorf("bootstrap server mapping %s configured twice: %v and %v", v.BrokerAddress, v, lc.ToListenerConfig())
			}
			continue
		}
		logrus.Infof("Bootstrap server %s advertised as %s", v.BrokerAddress, v.AdvertisedAddress)
		brokerToListenerConfig[v.BrokerAddress] = FromListenerConfig(v)
	}

	externalToListenerConfig := make(map[string]config.ListenerConfig)
	for _, v := range cfg.Proxy.ExternalServers {
		if lc, ok := externalToListenerConfig[v.BrokerAddress]; ok {
			if lc.ListenerAddress != v.ListenerAddress {
				return nil, fmt.Errorf("external server mapping %s configured twice: %s and %v", v.BrokerAddress, v.ListenerAddress, lc)
			}
			continue
		}
		if v.ListenerAddress != v.AdvertisedAddress {
			return nil, fmt.Errorf("external server mapping has different listener and advertised addresses %v", v)
		}
		externalToListenerConfig[v.BrokerAddress] = v
	}

	for _, v := range externalToListenerConfig {
		if lc, ok := brokerToListenerConfig[v.BrokerAddress]; ok {
			if lc.AdvertisedAddress != v.AdvertisedAddress {
				return nil, fmt.Errorf("bootstrap and external server mappings %s with different advertised addresses: %v and %v", v.BrokerAddress, v.ListenerAddress, lc.AdvertisedAddress)
			}
			continue
		}
		logrus.Infof("External server %s advertised as %s", v.BrokerAddress, v.AdvertisedAddress)
		brokerToListenerConfig[v.BrokerAddress] = FromListenerConfig(v)
	}
	return brokerToListenerConfig, nil
}

// GetAdvertisedListener uses the upstream broker host, port and id to identify the associated advertised listener.
func (p *Listeners) GetAdvertisedListener(brokerHost string, brokerPort int32, brokerId int32) (advertisedHost string, advertisedPort int32, err error) {

	// fmt.Println("proxy/proxy.go:", "Listeners.GetListenerAddress", "brokerHost", brokerHost, "brokerPort", brokerPort, "brokerId", brokerId)
	if brokerHost == "" || brokerPort <= 0 {
		return "", 0, fmt.Errorf("broker address '%s:%d' is invalid", brokerHost, brokerPort)
	}

	brokerAddress := net.JoinHostPort(brokerHost, fmt.Sprint(brokerPort))

	p.lock.RLock()
	listenerConfig, ok := p.brokerToListenerConfig[brokerAddress]
	p.lock.RUnlock()

	if ok {
		logrus.Debugf("Address mappings broker=%s, listener=%s, advertised=%s, brokerId=%d", listenerConfig.GetBrokerAddress(), listenerConfig.ListenerAddress, listenerConfig.AdvertisedAddress, brokerId)
		return util.SplitHostPort(listenerConfig.AdvertisedAddress)
	}

	if !p.disableDynamicListeners {
		logrus.Infof("Starting dynamic listener for broker %s", brokerAddress)
		return p.ListenDynamicInstance(brokerAddress, brokerId)
	}

	if len(p.dynamicAdvertisedListener) == 0 {
		return "", 0, fmt.Errorf("net address mapping for %s:%d was not found", brokerHost, brokerPort)
	}

	p.lock.Lock()
	defer p.lock.Unlock()
	dynamicAdvertisedHost, dynamicAdvertisedPort, err := p.getDynamicAdvertisedAddress(brokerId, int(brokerPort))
	if err != nil {
		return "", 0, err
	}
	// TODO: what should be the p.defaultListenerIP for the listener?
	cfg := NewListenerConfig(
		brokerAddress,
		net.JoinHostPort(p.defaultListenerIP, fmt.Sprintf("%v", dynamicAdvertisedPort)),
		net.JoinHostPort(dynamicAdvertisedHost, fmt.Sprint(dynamicAdvertisedPort)),
		brokerId,
	)
	p.brokerToListenerConfig[brokerAddress] = cfg
	return util.SplitHostPort(cfg.GetAdvertisedAddress())
}

func (p *Listeners) findListenerConfig(brokerId int32) *ListenerConfig {
	for _, listenerConfig := range p.brokerToListenerConfig {
		if listenerConfig.BrokerID == brokerId {
			return listenerConfig
		}
	}
	return nil
}

// ListenDynamicInstance creates a new listener for the upstream broker address and broker id.
func (p *Listeners) ListenDynamicInstance(brokerAddress string, brokerId int32) (string, int32, error) {

	fmt.Println("proxy/proxy.go: ListenDynamicInstance:", "brokerAddress", brokerAddress, "brokerId", brokerId)
	p.lock.Lock()
	defer p.lock.Unlock()
	// double check
	if v, ok := p.brokerToListenerConfig[brokerAddress]; ok {
		return util.SplitHostPort(v.AdvertisedAddress)
	}

	var listenerAddress string
	if p.deterministicListeners {
		if brokerId < 0 {
			return "", 0, fmt.Errorf("brokerId is negative %s %d", brokerAddress, brokerId)
		}
		deterministicPort := p.dynamicSequentialMinPort + int(brokerId)
		if deterministicPort < p.dynamicSequentialMinPort {
			return "", 0, fmt.Errorf("port assignment overflow %s %d: %d", brokerAddress, brokerId, deterministicPort)
		}
		listenerAddress = net.JoinHostPort(p.defaultListenerIP, strconv.Itoa(deterministicPort))
		cfg := p.findListenerConfig(brokerId)
		if cfg != nil {
			oldBrokerAddress := cfg.GetBrokerAddress()
			if oldBrokerAddress != brokerAddress {
				delete(p.brokerToListenerConfig, oldBrokerAddress)
				cfg.SetBrokerAddress(brokerAddress)
				p.brokerToListenerConfig[brokerAddress] = cfg
				logrus.Infof("Broker address changed listener %s for new address %s old address %s brokerId %d advertised as %s", cfg.ListenerAddress, cfg.GetBrokerAddress(), oldBrokerAddress, cfg.BrokerID, cfg.AdvertisedAddress)
			}
			return util.SplitHostPort(cfg.AdvertisedAddress)
		}
	} else {

		listenerAddress = net.JoinHostPort(p.defaultListenerIP, strconv.Itoa(p.dynamicSequentialMinPort))
		if p.dynamicSequentialMinPort != 0 {
			p.dynamicSequentialMinPort += 1
		}
	}

	cfg := NewListenerConfig(brokerAddress, listenerAddress, "", brokerId)
	l, err := p.listenInstance(p.connSrc, cfg, p.tcpConnOptions, p.listenFunc, p)
	if err != nil {
		return "", 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	address := net.JoinHostPort(p.defaultListenerIP, fmt.Sprint(port))

	dynamicAdvertisedHost, dynamicAdvertisedPort, err := p.getDynamicAdvertisedAddress(cfg.BrokerID, port)
	if err != nil {
		return "", 0, err
	}
	cfg.AdvertisedAddress = net.JoinHostPort(dynamicAdvertisedHost, fmt.Sprint(dynamicAdvertisedPort))
	cfg.ListenerAddress = address

	p.brokerToListenerConfig[brokerAddress] = cfg
	logrus.Infof("Dynamic listener %s for broker %s brokerId %d advertised as %s", cfg.ListenerAddress, cfg.GetBrokerAddress(), cfg.BrokerID, cfg.AdvertisedAddress)

	return dynamicAdvertisedHost, int32(dynamicAdvertisedPort), nil
}

func (p *Listeners) getDynamicAdvertisedAddress(brokerID int32, port int) (string, int, error) {
	dynamicAdvertisedListener := p.dynamicAdvertisedListener
	if dynamicAdvertisedListener == "" {
		return p.defaultListenerIP, port, nil
	}
	dynamicAdvertisedListener, err := p.templateDynamicAdvertisedAddress(brokerID)
	if err != nil {
		return "", 0, err
	}
	var (
		dynamicAdvertisedHost = dynamicAdvertisedListener
		dynamicAdvertisedPort = port
	)
	advHost, advPortStr, err := net.SplitHostPort(dynamicAdvertisedListener)
	if err == nil {
		if advPort, err := strconv.Atoi(advPortStr); err == nil {
			dynamicAdvertisedHost = advHost
			dynamicAdvertisedPort = advPort
		}
	}
	return dynamicAdvertisedHost, dynamicAdvertisedPort, nil
}

func (p *Listeners) templateDynamicAdvertisedAddress(brokerID int32) (string, error) {
	tmpl, err := template.New("dynamicAdvertisedHost").Option("missingkey=error").Parse(p.dynamicAdvertisedListener)
	if err != nil {
		return "", fmt.Errorf("failed to parse host template '%s': %w", p.dynamicAdvertisedListener, err)
	}
	var buf bytes.Buffer
	data := map[string]any{
		"brokerId": brokerID,
		"brokerID": brokerID,
	}
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute host template '%s': %w", p.dynamicAdvertisedListener, err)
	}
	return buf.String(), nil
}

func (p *Listeners) GetBrokerAddressByAdvertisedHost(host string) (brokerAddress string, brokerId int32, err error) {

	// TODO: Use a better algorithm than a forloop
	// Maybe use a bi-directional map? I believe downstream hostport always has only one upstream hostport.
	for brokerAddr, c := range p.brokerToListenerConfig {
		if c == nil {
			continue
		}
		// fmt.Println("Comparing", "host", host, "advertisedAddress", c.GetAdvertisedAddress())
		advertisedHost, _, err := net.SplitHostPort(c.GetAdvertisedAddress())
		if err != nil {
			fmt.Println("failed to split address", err)
		}
		if advertisedHost == host {
			// fmt.Println("Match!", "brokerAddr", brokerAddr, "brokerID", c.GetBrokerID(), "advertisedAddress", c.GetAdvertisedAddress())
			return brokerAddr, c.GetBrokerID(), nil
		}
	}

	return "", 0, fmt.Errorf("broker not found for advertised host %q", host)
}

// ListenInstances creates listeners from static configuration.
func (p *Listeners) ListenInstances(cfgs []config.ListenerConfig) (<-chan Conn, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// allows multiple local addresses to point to the remote
	for _, v := range cfgs {
		cfg := FromListenerConfig(v)
		_, err := p.listenInstance(p.connSrc, cfg, p.tcpConnOptions, p.listenFunc, p)
		if err != nil {
			return nil, err
		}
	}
	return p.connSrc, nil
}

type BrokerConfigMap interface {
	ToListenerConfig() config.ListenerConfig
	GetListenerAddress() string
	GetBrokerAddress() string
	GetAdvertisedAddress() string
	GetBrokerID() int32
}

type HostBasedRouting interface {
	GetBrokerAddressByAdvertisedHost(host string) (brokerAddress string, brokerId int32, err error)
}

func PrintTLV(t proxyproto.PP2Type) string {
	switch t {
	case proxyproto.PP2_TYPE_ALPN:
		return "PP2_TYPE_ALPN"
	case proxyproto.PP2_TYPE_AUTHORITY:
		return "PP2_TYPE_AUTHORITY"
	case proxyproto.PP2_TYPE_CRC32C:
		return "PP2_TYPE_CRC32C"
	case proxyproto.PP2_TYPE_NOOP:
		return "PP2_TYPE_NOOP"
	case proxyproto.PP2_TYPE_UNIQUE_ID:
		return "PP2_TYPE_UNIQUE_ID"
	case proxyproto.PP2_TYPE_SSL:
		return "PP2_TYPE_SSL"
	case proxyproto.PP2_SUBTYPE_SSL_VERSION:
		return "PP2_SUBTYPE_SSL_VERSION"
	case proxyproto.PP2_SUBTYPE_SSL_CN:
		return "PP2_SUBTYPE_SSL_CN"
	case proxyproto.PP2_SUBTYPE_SSL_CIPHER:
		return "PP2_SUBTYPE_SSL_CIPHER"
	case proxyproto.PP2_SUBTYPE_SSL_SIG_ALG:
		return "PP2_SUBTYPE_SSL_SIG_ALG"
	case proxyproto.PP2_SUBTYPE_SSL_KEY_ALG:
		return "PP2_SUBTYPE_SSL_KEY_ALG"
	case proxyproto.PP2_TYPE_NETNS:
		return "PP2_TYPE_NETNS"
	case proxyproto.PP2_TYPE_MIN_CUSTOM:
		return "PP2_TYPE_MIN_CUSTOM"
	case proxyproto.PP2_TYPE_MAX_CUSTOM:
		return "PP2_TYPE_MAX_CUSTOM"
	case proxyproto.PP2_TYPE_MIN_EXPERIMENT:
		return "PP2_TYPE_MIN_EXPERIMENT"
	case proxyproto.PP2_TYPE_MAX_EXPERIMENT:
		return "PP2_TYPE_MAX_EXPERIMENT"
	case proxyproto.PP2_TYPE_MIN_FUTURE:
		return "PP2_TYPE_MIN_FUTURE"
	case proxyproto.PP2_TYPE_MAX_FUTURE:
		return "PP2_TYPE_MAX_FUTURE"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func (p *Listeners) listenInstance(dst chan<- Conn, cfg BrokerConfigMap, opts TCPConnOptions, listenFunc ListenFunc, brokers HostBasedRouting) (net.Listener, error) {

	l, err := listenFunc(cfg.GetListenerAddress())
	if err != nil {
		return nil, err
	}
	go withRecover(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				logrus.Infof("Error in accept for %q on %v: %v", cfg.ToListenerConfig(), l.Addr(), err)
				l.Close()
				return
			}

			// Since we support multiple encapsulated listeners, we unpack connections.

			var (
				clientServerName string
				clientIP         string
				underlyingConn   net.Conn = c
			)

			// When using proxy protocol, the first bytes written to the tcp connection
			// will contain the proxy protocol headers.
			// It is entirely possible to have a proxy protocol v2 encapsulating a tls
			// connection.
			if conn, ok := underlyingConn.(*proxyproto.Conn); ok {
				header := conn.ProxyHeader()
				if header == nil {
					logrus.Error("unable to read the proxy protocol headers")
					c.Close()
					return
				}
				clientIP = header.SourceAddr.String()

				// Inspect optional TLVs for a forwarded SNI.
				tlvs, err := header.TLVs()
				if err != nil {
					logrus.Errorf("unable to read proxy protocol TLVs")
				}
				for _, tlv := range tlvs {
					if tlv.Type == proxyproto.PP2_TYPE_AUTHORITY {
						clientServerName = string(tlv.Value)
						logrus.Debugf("Discovered proxy protocol v2 authority: %s", clientServerName)
						break
					}
				}
				underlyingConn = conn.Raw()
			}

			if conn, ok := underlyingConn.(*tls.Conn); ok {
				err := conn.Handshake()
				if err != nil {
					logrus.Error("tls handshake failed: %s", err.Error())
					c.Close()
					return
				}

				tlsState := conn.ConnectionState()
				if clientServerName == "" {
					logrus.Debugf("Discoverd server name from tls client hello: %s", clientServerName)
					clientServerName = tlsState.ServerName
				} else {
					logrus.Debugf("Using proxy protocol authority %q instead of tls sni %q", clientServerName, tlsState.ServerName)
				}

				underlyingConn = conn.NetConn()
			}

			if conn, ok := underlyingConn.(*net.TCPConn); ok {
				if err := opts.setTCPConnOptions(conn); err != nil {
					logrus.Infof("WARNING: Error while setting TCP options for accepted connection %q on %v: %v", cfg.ToListenerConfig(), l.Addr().String(), err)
				}
			}

			brokerAddress := cfg.GetBrokerAddress()
			brokerId := cfg.GetBrokerID()

			if clientServerName != "" {
				if address, id, err := brokers.GetBrokerAddressByAdvertisedHost(clientServerName); err == nil {
					brokerAddress = address
					brokerId = id
				} else {
					logrus.Infof("%s: Failed to match host/authority %q with any advertised address", l.Addr(), clientServerName)
				}
			}

			var client string
			if clientIP != "" {
				client = fmt.Sprintf("%s via %s", clientIP, c.RemoteAddr().String())
			} else {
				client = c.RemoteAddr().String()
			}

			if brokerId != UnknownBrokerID {
				logrus.Infof("%s: New connection from %q for %s brokerId %d with server name %s",
					l.Addr(),
					client,
					brokerAddress,
					brokerId,
					clientServerName,
				)
			} else {
				logrus.Infof("%s: New connection from %q with server name %q",
					l.Addr(),
					client,
					clientServerName,
				)
			}
			dst <- Conn{BrokerAddress: brokerAddress, LocalConnection: c}
		}
	})
	if cfg.GetBrokerID() != UnknownBrokerID {
		logrus.Infof("Listening on %s for remote %s broker %d", l.Addr().String(), cfg.GetBrokerAddress(), cfg.GetBrokerID())
	} else {
		logrus.Infof("Listening on %s for remote %s", l.Addr().String(), cfg.GetBrokerAddress())
	}
	return l, nil
}
