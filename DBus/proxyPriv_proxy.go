package DBus

import (
	com "github.com/DeepinProxy/Com"
	config "github.com/DeepinProxy/Config"
	tProxy "github.com/DeepinProxy/TProxy"
	"github.com/godbus/dbus"
	"net"
	"pkg.deepin.io/lib/dbusutil"
	"strconv"
)

// interface path
func (mgr *proxyPrv) GetInterfaceName() string {
	return BusInterface + "." + mgr.scope.ToString()
}

// get proxy
func (mgr *proxyPrv) GetProxy() (string, *dbus.Error) {
	buf, err := com.MarshalJson(mgr.Proxy)
	if err != nil {
		logger.Warningf("[%s] get proxy failed, err: %v", mgr.scope, err)
		return "",dbusutil.ToError(err)
	}
	return buf, nil
}

// start proxy
func (mgr *proxyPrv) StartProxy(proto string, name string, udp bool) *dbus.Error {
	logger.Debugf("[%s] start proxy, proto [%s] name [%s] udp [%v]", mgr.scope, proto, name, udp)
	// check if proto is legal
	var proxyTyp tProxy.ProtoTyp
	var err error
	if proto == "sock5" {
		// never err
		proxyTyp = tProxy.SOCK5TCP
	} else {
		proxyTyp, err = tProxy.BuildProto(proto)
		if err != nil {
			return dbusutil.ToError(err)
		}
	}
	// get proxies
	proxy, err := mgr.Proxies.GetProxy(proto, name)
	if err != nil {
		logger.Warningf("[%s] get proxy failed, err: %v", mgr.scope, err)
		return dbusutil.ToError(err)
	}
	// save proxy
	mgr.Proxy = proxy
	logger.Debugf("[%s] get proxy success, proxy: %v", mgr.scope, proxy)
	// tcp module
	listen, err := mgr.listen()
	if err != nil {
		return dbusutil.ToError(err)
	}
	logger.Debugf("[%s] proxy [%s] listen tcp success at port %v", mgr.scope, proto, mgr.Proxies.TPort)
	// in case blocks DBus-return, use goroutine
	go mgr.accept(proxyTyp, proxy, listen)

	// udp module
	if udp && proto == "sock5" {
		// listen packet conn
		packetConn, err := mgr.listenPacket()
		if err != nil {
			return dbusutil.ToError(err)
		}
		logger.Debugf("[%s] proxy [%s] listen udp success at port %v", mgr.scope, proto, mgr.Proxies.TPort)
		// start proxy udp
		go mgr.readMsgUDP(proxyTyp, proxy, packetConn)
	}

	go func() {
		_ = mgr.startRedirect()
	}()

	// mark enable
	mgr.Enabled = true

	return nil
}

// stop proxy
func (mgr *proxyPrv) StopProxy() *dbus.Error {
	logger.Debugf("[%s] stop proxy, enable: %v, proxy: %v", mgr.scope, mgr.Enabled, mgr.Proxy)
	// stop to break accept and read message
	mgr.stop.Broadcast()

	go func() {
		_ = mgr.stopRedirect()
	}()

	mgr.Enabled = false
	return nil
}

// set proxy
func (mgr *proxyPrv) AddProxy(proto string, name string, jsonProxy []byte) *dbus.Error {
	proxy, err := UnMarshalProxy(jsonProxy)
	if err != nil {
		logger.Warningf("[%s] unmarshal proxy message failed, err: %v", mgr.scope, err)
		return dbusutil.ToError(err)
	}
	// check if exist
	mgr.Proxies.SetProxy(proto, name, proxy)
	return nil
}

// set proxies
func (mgr *proxyPrv) SetProxies(proxies config.ScopeProxies) *dbus.Error {
	mgr.Proxies = proxies
	err := mgr.writeConfig()
	if err != nil {
		logger.Warningf("[%s] write config failed, err: %v", mgr.scope, err)
		return dbusutil.ToError(err)
	}
	return nil
}

func (mgr *proxyPrv) ClearProxies() *dbus.Error {
	mgr.Proxies = config.ScopeProxies{}
	err := mgr.writeConfig()
	if err != nil {
		logger.Warningf("[%s] write config failed, err: %v", mgr.scope, err)
		return dbusutil.ToError(err)
	}
	return nil
}

// set tcp opt listen
func (mgr *proxyPrv) listen() (net.Listener, error) {
	// get proxies
	tp := strconv.Itoa(mgr.Proxies.TPort)
	l, err := net.Listen("tcp", ":"+tp)
	if err != nil {
		logger.Warningf("[%s] listen port failed, err: %v", mgr.scope, err)
		return nil, err
	}
	// convert to tcp listener
	tl, ok := l.(*net.TCPListener)
	if !ok {
		logger.Warningf("[%s] listener is not tcp listener type", mgr.scope)
		return nil, err
	}
	// get file
	file, err := tl.File()
	if err != nil {
		logger.Warningf("[%s] tcp listener get file failed, err: %v", err)
		return nil, err
	}
	// set transparent
	err = com.SetSockOptTrn(int(file.Fd()))
	if err != nil {
		logger.Warningf("[%s] set fd opt transparent failed, err: %v", mgr.scope, err)
		return nil, err
	}
	return l, nil
}

// set udp opt listen
func (mgr *proxyPrv) listenPacket() (net.PacketConn, error) {
	// get proxies
	tp := strconv.Itoa(mgr.Proxies.TPort)
	l, err := net.ListenPacket("udp", ":"+tp)
	if err != nil {
		logger.Warningf("[%s] listen udp package port failed, err: %v", mgr.scope, err)
		return nil, err
	}
	// ip_transparent
	conn, ok := l.(*net.UDPConn)
	if !ok {
		logger.Warning("convert udp data failed")
		return nil, err
	}
	err = com.SetConnOptTrn(conn)
	if err != nil {
		logger.Warningf("set conn opt transparent failed, err: %v", err)
		return nil, err
	}
	return l, nil
}

// proxy tcp
func (mgr *proxyPrv) accept(proxyTyp tProxy.ProtoTyp, proxy config.Proxy, listen net.Listener) {
	if listen == nil {
		logger.Warningf("[%s] tcp listener is nil", mgr.scope)
		return
	}

	// wait stop
	ch := make(chan bool)
	go func() {
		mgr.stop.L.Lock()
		mgr.stop.Wait()
		mgr.stop.L.Unlock()
		ch <- true
	}()

	// start accept until stop
	for {
		select {
		case <-ch:
			// close all scope handler
			mgr.handlerMgr.CloseTypHandler(proxyTyp)
			// break accept
			break
		default:
			// accept connect
			lConn, err := listen.Accept()
			if err != nil {
				logger.Warningf("[%s] accept socket failed, err: %v", proxyTyp, err)
				continue
			}
			// proxy tcp
			go mgr.proxyTcp(proxyTyp, proxy, lConn)
		}
	}
}

// read udp message
func (mgr *proxyPrv) readMsgUDP(proxyTyp tProxy.ProtoTyp, proxy config.Proxy, listen net.PacketConn) {
	if listen == nil {
		logger.Warningf("[%s] tcp listener is nil", mgr.scope)
		return
	}
	// ip_transparent
	conn, ok := listen.(*net.UDPConn)
	if !ok {
		logger.Warning("convert udp data failed")
		return
	}

	// wait stop
	ch := make(chan bool)
	go func() {
		mgr.stop.L.Lock()
		mgr.stop.Wait()
		mgr.stop.L.Unlock()
		ch <- true
	}()

	// start accept until stop
	for {
		select {
		case <-ch:
			// close all scope handler
			mgr.handlerMgr.CloseTypHandler(proxyTyp)
			// break accept
			break
		default:
			// read origin addr
			buf := make([]byte, 512)
			oob := make([]byte, 1024)
			_, oobNum, _, lAddr, err := conn.ReadMsgUDP(buf, oob)
			if err != nil {
				logger.Fatal(err)
			}
			// get real remote addr
			rBaseAddr, err := com.ParseRemoteAddrFromMsgHdr(oob[:oobNum])
			if err != nil {
				logger.Fatal(err)
			}

			// make remote addr
			rAddr := &net.UDPAddr{
				IP:   rBaseAddr.IP,
				Port: rBaseAddr.Port,
			}
			// proxy udp
			go mgr.proxyUdp(proxy, lAddr, rAddr, buf)
		}
	}
}

func (mgr *proxyPrv) proxyTcp(proxyTyp tProxy.ProtoTyp, proxy config.Proxy, lConn net.Conn) {
	// request is redirect by t-proxy, output -> pre-routing
	// at that time, the actual remote addr is conn`s local addr, the actual local addr is conn`s remote addr
	// can use conn as fake remote conn, to connect with actual local connection
	lAddr := lConn.RemoteAddr()
	rAddr := lConn.LocalAddr()

	// print local -> remote
	logger.Debugf("[%s] tcp request capture by proxy successfully, "+
		"local[%s] -> remote [%s]", proxyTyp, lAddr.String(), rAddr.String())

	// make key to mark this connection
	key := tProxy.HandlerKey{
		SrcAddr: lAddr.String(),
		DstAddr: rAddr.String(),
	}
	// create new handler
	handler := tProxy.NewHandler(proxyTyp, mgr.scope, key, proxy, lAddr, rAddr, lConn)
	// create tunnel between proxy server and dst server
	err := handler.Tunnel()
	if err != nil {
		logger.Warningf("[%s] create tunnel failed, err: %v", proxyTyp, err)
		handler.Close()
		return
	}
	// add handler to map
	handler.AddMgr(mgr.handlerMgr)
	// begin communication
	handler.Communicate()
}

func (mgr *proxyPrv) proxyUdp(proxy config.Proxy, lAddr net.Addr, rAddr net.Addr, buf []byte) {
	// make a fake udp dial to cheat socket
	lConn, err := com.MegaDial("udp", rAddr, lAddr)
	if err != nil {
		logger.Warningf("fake dial udp rAddr to lAddr failed, err: %v", err)
		return
	}
	// make key to mark this connection
	key := tProxy.HandlerKey{
		SrcAddr: lAddr.String(),
		DstAddr: rAddr.String(),
	}
	// create new handler
	handler := tProxy.NewHandler(tProxy.SOCK5UDP, mgr.scope, key, proxy, lAddr, rAddr, lConn)
	// create tunnel between proxy server and dst server
	err = handler.Tunnel()
	if err != nil {
		logger.Warningf("[%s] create tunnel failed, err: %v", tProxy.SOCK5UDP, err)
		handler.Close()
		return
	}
	// add handler to map
	handler.AddMgr(mgr.handlerMgr)
	// begin communication
	handler.Communicate()
	// write first buf to rAddr
	pkgData := com.DataPackage{
		Addr: rAddr,
		Data: buf,
	}
	// write first udp to remote
	err = handler.WriteRemote(com.MarshalPackage(pkgData, "udp"))
	if err != nil {
		handler.Close()
		return
	}
}
