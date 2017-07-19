
package main

import (
	"github.com/lab11/go-tuntap/tuntap"
	"log"
	"fmt"
	"os/exec"
	//"encoding/binary"
	"flag"
	"net"
	"time"
	"io/ioutil"
	"packet"
	"mylog"
	"crypto/tls"
	"net/http"
	_"net/http/pprof"
	"reflect"
	"encoding/binary"
	"io"
	"bufio"
	"sync"
)

var (
	buildTime string
	br = flag.String("br", ""," add tun/tap to bridge")
	tuntype = flag.Int("tuntype", int(tuntap.DevTap)," type, 1 means tap and 0 means tun")
	tunname = flag.String("tundev","tap0"," tun dev name")
	server = flag.String("server",":7878"," server like 203.156.34.98:7878")
	tlsEnable = flag.Bool("tls", false, "enable tls connect")
	pprofEnable = flag.Bool("pprof", false, "enable pprof, true or false")
	ppAddr = flag.String("ppaddr", ":7070", "ppaddr , http://xxxx:7070/debug/pprof/")
	chanSize = flag.Int("chanSize", 4096, "chan Size")
	lnAddr = flag.String("lnAddr",""," server like 203.156.34.98:7878")
	DebugEn = flag.Bool("DebugEn", false, "debug, show ip packet information")
	ipstr = flag.String("ipstr", "", "set tun/tap or br ip address")
)
/*
go tool pprof http://localhost:7070/debug/pprof/heap
go tool pprof http://localhost:7070/debug/pprof/profile
web
*/

func cmdexec (cmds string, checkErr bool){
	if !checkErr{
		exec.Command("sh", "-c", cmds).Run()
		return
	}
	cmd := exec.Command("sh", "-c", cmds)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	slurp, _ := ioutil.ReadAll(stderr)
	fmt.Printf("%s\n", slurp)

	if err := cmd.Wait(); err != nil {
		log.Fatal(err)
	}
}

type myio interface {
	Open()
	Read()	
	PutPktToChan(pkt packet.Packet)
	WriteFromChan()
	SetPeer( peer myio) bool
	IsClose() bool
}

type mytun struct {
	tund *tuntap.Interface
	pktchan chan packet.Packet
	peer myio	
}

type myconn struct {
	conn net.Conn
	pktchan chan packet.Packet
	writeQuit chan bool
	reconnect chan bool
	isClosed bool
	peer myio
	sync.Mutex	
}

func NewTun() *mytun{
	return &mytun {
		pktchan : make(chan packet.Packet, *chanSize),
	}
}

func (tun *mytun) Open() {	
	var err error
	tun.tund, err = tuntap.Open(*tunname, tuntap.DevKind(*tuntype) , false)
	if err != nil {
		log.Fatal(err)
	}

	confs := fmt.Sprintf("ifconfig %s up\n", *tunname)
	
	if *br != "" {
		confs += fmt.Sprintf("brctl addbr %s\n", *br)
		confs += fmt.Sprintf("brctl addif %s %s\n", *br, *tunname)	
		if *ipstr != "" {
			confs += fmt.Sprintf("ifconfig %s %s\n", *br, *ipstr)
		}
	} else if *ipstr != "" {
		confs += fmt.Sprintf("ifconfig %s %s\n", *tunname, *ipstr)
	}
	err = exec.Command("sh","-c", confs).Run()
	if err != nil {
		log.Printf("open err:%s, confs = %s \n", err.Error(), confs)
		log.Fatal(err)
	}
	log.Printf("================%s open ==========\n", *tunname)
}

func (tun *mytun) Read() {
	pktLen := 0
	for {
		data := make([]byte, 2048)
		inpkt, err := tun.tund.ReadPacket2(data[2:])
		if err != nil{
			log.Println("==============tund.ReadPacket error===", err)
			log.Fatal(err)
			return
		}
		pktLen = len(inpkt.Packet)
		if /*pktLen < 42 ||*/ pktLen > 1514 {
			log.Printf("======tun read len=%d out of range =======\n", pktLen)
			continue
		}

		if *tuntype == 1 {
			if *DebugEn {
				ether := packet.TranEther(inpkt.Packet)
				if  ether.IsBroadcast() && ether.IsArp() {
					log.Println("---------arp broadcast from tun/tap ----------")
					log.Printf("dst mac :%s", ether.DstMac.String())
					log.Printf("src mac :%s", ether.SrcMac.String())
				}
				/*
				if !ether.IsArp() && !ether.IsIpPtk() {
					//mylog.Warning(" not arp ,and not ip packet, ether type =0x%02x===============\n", ether.Proto)
					continue
				}*/

				if  ether.IsIpPtk() {
					iphdr, err := packet.ParseIPHeader(inpkt.Packet[packet.EtherSize:])
					if err != nil {
						log.Printf("ParseIPHeader err: %s\n", err.Error())
					}
					fmt.Println("tun read: ", iphdr.String())
				}
			}
		} else {
			if *DebugEn {
				iphdr, err := packet.ParseIPHeader(inpkt.Packet)
				if err != nil {
					log.Printf("ParseIPHeader err: %s\n", err.Error())
				}
				fmt.Println("tun read: ", iphdr.String())
			}			
		}

		binary.BigEndian.PutUint16(data[:2], uint16(pktLen))
		copy(data[2:], inpkt.Packet[:pktLen])
		tun.FwdToPeer(data[:pktLen+2])
	}
}

func PutPktToChan (pkt packet.Packet, mi myio) {
	mi.PutPktToChan(pkt)
}

func (tun *mytun) GetPeer() myio {
	return tun.peer
}

func (tun *mytun) FwdToPeer(pkt packet.Packet) {
	if tun.peer != nil && !tun.peer.IsClose() {
		tun.peer.PutPktToChan(pkt)
	}	
}

func (tun *mytun) PutPktToChan(pkt packet.Packet) {
	tun.pktchan <- pkt
}

func (tun *mytun) WriteFromChan() {
	for pkt := range tun.pktchan {		
		inpkt := &tuntap.Packet{Packet: pkt[:]}
		err := tun.tund.WritePacket(inpkt)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func (tun *mytun) IsClose() bool {
	return false
}

func  NewConn() *myconn{
	return &myconn{
		pktchan : make(chan packet.Packet, *chanSize),
		writeQuit : make(chan bool, 1),
		reconnect : make(chan bool, 1),
		isClosed : false,
	}
}

func (c *myconn) Open() {
	var err error
	n := 1
	ReConnect:

	if *tlsEnable {
		tlsconf := &tls.Config{
 			InsecureSkipVerify: true,
 		}
 		c.conn, err  = tls.Dial("tcp", *server, tlsconf)
	}else {
		c.conn, err = net.Dial("tcp4", *server)		
	}

	if err != nil {
		log.Printf("try to connect to  %s time =%d\n", *server, n)
		n += 1
		fmt.Println(err.Error())
		time.Sleep(time.Second * 2)
		goto ReConnect
	}
	
	if tcpConn, ok := c.conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
	}

	log.Println("success ,clinet:", c.conn.LocalAddr().String(),"connect to Server:", c.conn.RemoteAddr())
}

func (c *myconn) Read() {
	defer c.Reconnect()
	pkt := make(packet.Packet, 65536)
	cr := bufio.NewReader(c.conn)
	for {
		// if err := binary.Read(cr, binary.BigEndian, &pktLen); err != nil {
		// 	log.Println("conn read fail:", err.Error())
		// 	c.Close()
		// 	break			
		// }	
		lenBuf, err := cr.Peek(2)
		if err != nil {
			log.Println("conn read fail:", err.Error())			
			break
		}

		pktLen := int(binary.BigEndian.Uint16(lenBuf))
		if /*pktLen < 42 ||*/ pktLen > 1514 {
			log.Panicf("parase pktLen=%d out of range \n", pktLen)			
		}

		rn, err := io.ReadFull(cr, pkt[:pktLen+2])
		if err != nil {
			log.Printf("ReadFull fail: %s, rn=%d, want=%d\n", err.Error(), rn, pktLen+2)
			break
		}

		data := make([]byte, pktLen)
		copy(data, pkt[2:pktLen+2])
		c.FwdToPeer(data)		
	}
}
/*
func (c *myconn) Read() {
	var pktStart, pktEnd int
	pkt := make(packet.Packet, 65536)
	last := struct {
		buf []byte
		pktLen int
		needMore int
	}{make([]byte, 1514), 0, 0}

	for {
		len , err := c.conn.Read(pkt)
		if err != nil{
			log.Println("conn.Read error: ",err)
			c.Reconnect()
			break
		}		
		if len < 42 {
			mylog.Warning("====== conn.read too small pkt, len=%d===========\n", len)
			continue
		}
		// TODO packet combine
		// if *DebugEn && len > 1500 {
		// 	mylog.Warning("====== conn.read too big %d,maybe tcp packet combine===========\n", len)
		// }
		
		pktStart, pktEnd = 0, 0
		for n := 0; pktEnd < len; {
			//check the remaining work from last handle packet
			if last.needMore != 0  {
				if last.needMore <= len {					
					//make data and foward
					data := make([]byte, last.pktLen+last.needMore)
					copy(data, last.buf[:last.pktLen])
					copy(data[last.pktLen:], pkt[:last.needMore])
					c.FwdToPeer(data)
					//set pktEnd
					pktEnd = last.needMore
					//reset last
					last.needMore = 0
					continue
				}else {
					fmt.Printf("can't be here, last.needMore=%d, totall len=%d\n", last.needMore, len)
					last.needMore = 0
					break
				}
			}

			if pktEnd + 2 > len {
				fmt.Printf("something wrong: pktEnd=%d, totall len=%d\n", pktEnd, len)
				break;
				//panic("pktEnd + 2 > len")	
			}
			n =	int(binary.BigEndian.Uint16(pkt[pktEnd:]))
			if n < 42 || n > 1514 {
				log.Printf("======error parse: pkt len unormal, n=%d, totall len=%d===========\n", n, len)
				break;
			}
			pktStart = pktEnd + 2
			pktEnd = pktStart + n
			if pktEnd > len {
				//log.Printf("====== out of range, pktStart=%d, n=%d, pktEnd=%d, totall len=%d, handle it next read===========\n", 
				//				pktStart, n, pktEnd, len)
				copy(last.buf, pkt[pktStart:len])
				last.pktLen , last.needMore = len - pktStart, pktEnd - len
				break;
			}
			ether := packet.TranEther(pkt[pktStart:pktEnd])
			if ether.IsBroadcast() && ether.IsArp() {
				mylog.Info("%s","---------arp broadcast from server ----------")
				mylog.Info("dst mac :%s", ether.DstMac.String())
				mylog.Info("src mac :%s", ether.SrcMac.String())
			}
			if *DebugEn && ether.IsIpPtk() {
				iphdr, err := packet.ParseIPHeader(pkt[pktStart + packet.EtherSize:])
				if err != nil {
					mylog.Warning("ParseIPHeader err: %s\n",err.Error())
				}
				fmt.Printf("conn read len =%d:%s", len, iphdr.String())
			}
			//PutPktToChan(pkt, c.peer)
			data := make([]byte, n)
			copy(data, pkt[pktStart:pktEnd])
			c.FwdToPeer(data)
		}
	}
}
*/
func (c *myconn) GetPeer() myio {
	return c.peer
}
func (c *myconn) FwdToPeer(pkt packet.Packet) {
	if c.peer != nil {
		c.peer.PutPktToChan(pkt)
	}	
}

func (c *myconn) PutPktToChan(pkt packet.Packet) {
	c.pktchan <- pkt
}

func (c *myconn) WriteFromChan() {
	defer c.Close()
	for {
		select {
			case pkt, ok := <- c.pktchan:
				if !ok {
					log.Printf("%s -> %s pktchan closed, quit the writefromchan  goroutine\n",
						 c.conn.LocalAddr().String(), c.conn.RemoteAddr().String())
					log.Printf(" c.pktchan is closed, quit the writefromchan  goroutine\n")		
					return	
				}	
				len, err := c.conn.Write(pkt)
				if err != nil{
					log.Printf(" write len=%d, err=%s\n", len, err.Error())		
					return
				}
				ether := packet.TranEther(pkt)
				if *DebugEn && ether.IsIpPtk() {
					iphdr, err := packet.ParseIPHeader(pkt[packet.EtherSize:])
					if err != nil {
						log.Println(err.Error())
					}
					fmt.Printf("pkt(len=%d) send to %s, ipheader:%s\n", c.conn.RemoteAddr().String(), iphdr.String())
				}
			case q, ok := <-c.writeQuit:
				if !ok {
					log.Printf(" c.writeQuit is closed , quit the writefromchan  goroutine\n")		
				} else {
					log.Printf("chan write_quit recive message: quit=%v, ok=%v\n", q, ok)	
				}					
				log.Printf("%s -> %s WriteFromChan quit \n", c.conn.LocalAddr().String(), c.conn.RemoteAddr().String())				
				return
		}
	}
}

func (c *myconn) Close() {	
	c.Lock()
	if !c.isClosed {
		log.Printf("%s -> %s is  closing \n", c.conn.LocalAddr().String(), c.conn.RemoteAddr().String())
		c.isClosed = true
		c.Unlock()
		c.conn.Close()		
		c.writeQuit <- true
		time.Sleep(time.Millisecond * 10)
		close(c.pktchan)
		close(c.writeQuit)
		log.Printf("%s -> %s is  closed \n", c.conn.LocalAddr().String(), c.conn.RemoteAddr().String())
		return
	}
	c.Unlock()
}

func (c *myconn) Reconnect() {
	c.Close()
	c.reconnect <- true
}

func (c *myconn) IsClose() bool {
	return c.isClosed
}

func (c *myconn) SetPeer(peer myio) bool {
	c.peer = peer
	return true
}
func (tun *mytun) SetPeer(peer myio) bool {
	tun.peer = peer
	return true
}
func bind(a, b myio) {
	a.SetPeer(b)
	b.SetPeer(a)
	/*	
	if bindPeer(a, b) == false || bindPeer(b, a) == false {
		log.Fatal("setpeer error")
	}
	*/	
}
func bindPeer(a, b interface{}) bool{
	v := reflect.ValueOf(a)
	t := v.Type()
	if t.Kind() == reflect.Ptr {
		f := v.MethodByName("SetPeer")
		if f.IsValid() {
			res := f.Call([]reflect.Value{reflect.ValueOf(b)})
			if ret, ok := res[0].Interface().(bool); ok {
				return ret
			}
		}
	}
	return false
}

func main () {  
	flag.Parse()
	mylog.InitLog(mylog.LINFO)
	var ln net.Listener
	var err error
	log.Printf("buildTime =%s\n", buildTime)
	log.Printf("tun name =%s, br=%s server=%s, enable pprof %v, ppaddr=%s, chanSize=%d, lnAddr=%s \n", *tunname, *br,
					 *server, *pprofEnable, *ppAddr, *chanSize, *lnAddr)

	if *pprofEnable {
		go func() {
			log.Println(http.ListenAndServe(*ppAddr, nil))
		}()
	}	

    tun := NewTun()
	tun.Open()
	go tun.WriteFromChan()
	go tun.Read()

	if *lnAddr != "" {
		ln, err = net.Listen("tcp4", *lnAddr)
		if err != nil {
			log.Fatalln(err)
		}	
	}

	for {
		cc := NewConn() 	
		if ln != nil {
			log.Printf("%s listenning .......\n", *lnAddr)
			cc.conn, err = ln.Accept()
			if err != nil {
				log.Fatalln(err)
			}
			cc.conn.(*net.TCPConn).SetNoDelay(true)			
			log.Printf("new connect :%s ->  %s\n", cc.conn.RemoteAddr().String(), cc.conn.LocalAddr().String())
		} else {
			cc.Open()
		}	
		
		bind(cc, tun)
		go cc.WriteFromChan()
		go cc.Read()
		<-cc.reconnect
		close(cc.reconnect)
		time.Sleep(time.Millisecond * 100)
	}
}