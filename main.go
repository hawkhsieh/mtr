package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// HopStats 存储每个跳点的统计数据
type HopStats struct {
	Addr       net.IP
	Hostname   string
	Sent       int
	Received   int
	Last       time.Duration
	Best       time.Duration
	Worst      time.Duration
	Sum        time.Duration
	TTL        int
	LastStatus string
	mu         sync.Mutex
}

// Stats 返回当前的统计数据
func (hs *HopStats) Stats() (sent, received int, last, best, worst, avg time.Duration) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	
	sent = hs.Sent
	received = hs.Received
	last = hs.Last
	best = hs.Best
	worst = hs.Worst
	
	if hs.Received > 0 {
		avg = hs.Sum / time.Duration(hs.Received)
	}
	
	return
}

// Update 更新统计数据
func (hs *HopStats) Update(rtt time.Duration, status string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	
	hs.Sent++
	
	if status == "ok" {
		hs.Received++
		hs.Last = rtt
		hs.Sum += rtt
		
		if hs.Best == 0 || rtt < hs.Best {
			hs.Best = rtt
		}
		
		if rtt > hs.Worst {
			hs.Worst = rtt
		}
	}
	
	hs.LastStatus = status
}

// LossPercentage 计算丢包率
func (hs *HopStats) LossPercentage() float64 {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	
	if hs.Sent == 0 {
		return 0
	}
	
	return 100 - (float64(hs.Received) / float64(hs.Sent) * 100)
}

func main() {
	var (
		host       string
		maxHops    int
		interval   time.Duration
		timeout    time.Duration
		packetSize int
		count      int
		resolveIP  bool
	)

	flag.StringVar(&host, "host", "", "Target host")
	flag.IntVar(&maxHops, "max-hops", 30, "Maximum number of hops")
	flag.DurationVar(&interval, "interval", time.Second, "Interval between pings")
	flag.DurationVar(&timeout, "timeout", time.Second, "Timeout for each probe")
	flag.IntVar(&packetSize, "size", 56, "Packet size in bytes")
	flag.IntVar(&count, "count", 0, "Number of pings (0 means infinite)")
	flag.BoolVar(&resolveIP, "resolve", true, "Resolve IP addresses to hostnames")
	flag.Parse()

	if flag.NArg() > 0 {
		host = flag.Arg(0)
	}

	if host == "" {
		fmt.Println("Usage: go-mtr [options] host")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// 解析目标地址
	ipAddr, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		log.Fatalf("Could not resolve %s: %v", host, err)
	}

	fmt.Printf("MTR to %s (%s)\n", host, ipAddr)
	fmt.Println("Press Ctrl+C to quit.")

	// 创建ICMP连接
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		log.Fatalf("Error creating ICMP connection: %v", err)
	}
	defer conn.Close()

	// 处理中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	doneCh := make(chan struct{})

	// 初始化每个跳点的统计数据
	hopsStats := make([]*HopStats, maxHops+1)
	for i := 1; i <= maxHops; i++ {
		hopsStats[i] = &HopStats{TTL: i, Best: 0}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	
	// 启动显示结果的goroutine
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// 清屏
				fmt.Print("\033[H\033[2J")
				
				fmt.Printf("MTR to %s (%s), %d bytes packets\n", host, ipAddr, packetSize)
				fmt.Printf("%-3s %-20s %-30s %5s %6s %6s %6s %6s %6s\n",
					"TTL", "IP", "HOST", "LOSS%", "SENT", "RECV", "LAST", "AVG", "BEST")

				for i := 1; i <= maxHops; i++ {
					hs := hopsStats[i]
					
					if hs.Addr == nil {
						fmt.Printf("%-3d %-20s %-30s %5s %6d %6d %6s %6s %6s\n",
							i, "???", "???", "?", hs.Sent, hs.Received, "?", "?", "?")
						continue
					}
					
					hostname := "???"
					if resolveIP && hs.Hostname != "" {
						hostname = hs.Hostname
					} else if !resolveIP {
						hostname = hs.Addr.String()
					}
					
					sent, received, last, _, _, avg := hs.Stats()
					
					lastStr := "?"
					if received > 0 {
						lastStr = fmt.Sprintf("%.1f", float64(last)/float64(time.Millisecond))
					}
					
					avgStr := "?"
					if received > 0 {
						avgStr = fmt.Sprintf("%.1f", float64(avg)/float64(time.Millisecond))
					}
					
					bestStr := "?"
					if received > 0 {
						bestStr = fmt.Sprintf("%.1f", float64(hs.Best)/float64(time.Millisecond))
					}
					
					fmt.Printf("%-3d %-20s %-30s %5.1f %6d %6d %6s %6s %6s\n",
						i, hs.Addr.String(), hostname, hs.LossPercentage(),
						sent, received, lastStr, avgStr, bestStr)
				}
			case <-doneCh:
				return
			}
		}
	}()

	// 发送探测包
	go func() {
		seq := 0
		counter := 0
		
		for {
			select {
			case <-time.After(interval):
				seq++
				counter++
				
				if count > 0 && counter > count {
					close(doneCh)
					return
				}
				
				for ttl := 1; ttl <= maxHops; ttl++ {
					// 创建ICMP回显请求
					wb, err := (&icmp.Message{
						Type: ipv4.ICMPTypeEcho, Code: 0,
						Body: &icmp.Echo{
							ID:   os.Getpid() & 0xffff,
							Seq:  seq,
							Data: make([]byte, packetSize),
						},
					}).Marshal(nil)
					if err != nil {
						log.Printf("Error creating ICMP message: %v", err)
						continue
					}

					// 设置TTL
					if err := conn.IPv4PacketConn().SetTTL(ttl); err != nil {
						log.Printf("Error setting TTL: %v", err)
						continue
					}

					start := time.Now()
					
					// 向目标发送ICMP包
					if _, err := conn.WriteTo(wb, &net.IPAddr{IP: ipAddr.IP}); err != nil {
						log.Printf("Error sending packet: %v", err)
						hopsStats[ttl].Update(0, "error")
						continue
					}

					// 设置读取超时
					if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
						log.Printf("Error setting read deadline: %v", err)
						continue
					}

					// 读取回复
					rb := make([]byte, 1500)
					n, peer, err := conn.ReadFrom(rb)
					if err != nil {
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							hopsStats[ttl].Update(0, "timeout")
						} else {
							log.Printf("Error reading packet: %v", err)
							hopsStats[ttl].Update(0, "error")
						}
						continue
					}

					rtt := time.Since(start)
					
					// 解析ICMP回复
					rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), rb[:n])
					if err != nil {
						log.Printf("Error parsing ICMP message: %v", err)
						hopsStats[ttl].Update(0, "error")
						continue
					}

					// 处理不同类型的ICMP回复
					switch rm.Type {
					case ipv4.ICMPTypeTimeExceeded:
						// TTL超时
						if peerIP, ok := peer.(*net.IPAddr); ok {
							hs := hopsStats[ttl]
							if hs.Addr == nil {
								hs.Addr = peerIP.IP
								
								// 如果需要解析主机名
								if resolveIP {
									go func(hs *HopStats, ip net.IP) {
										names, err := net.LookupAddr(ip.String())
										if err == nil && len(names) > 0 {
											hs.mu.Lock()
											hs.Hostname = names[0]
											hs.mu.Unlock()
										}
									}(hs, peerIP.IP)
								}
							}
							hs.Update(rtt, "ok")
						}
						
					case ipv4.ICMPTypeEchoReply:
						// 回显应答 (到达目标)
						if peerIP, ok := peer.(*net.IPAddr); ok {
							hs := hopsStats[ttl]
							if hs.Addr == nil {
								hs.Addr = peerIP.IP
								
								// 如果需要解析主机名
								if resolveIP {
									go func(hs *HopStats, ip net.IP) {
										names, err := net.LookupAddr(ip.String())
										if err == nil && len(names) > 0 {
											hs.mu.Lock()
											hs.Hostname = names[0]
											hs.mu.Unlock()
										}
									}(hs, peerIP.IP)
								}
							}
							hs.Update(rtt, "ok")
							
							// 如果收到目标主机的回复，就不需要继续发送更大TTL的包
							break
						}
						
					default:
						// 其他ICMP消息
						hopsStats[ttl].Update(0, fmt.Sprintf("unknown: %v", rm.Type))
					}
				}
				
			case <-doneCh:
				return
			}
		}
	}()

	// 等待中断信号
	<-sigCh
	close(doneCh)
	wg.Wait()
	
	// 显示最终结果
	fmt.Print("\033[H\033[2J")
	fmt.Printf("MTR to %s (%s), %d bytes packets - FINAL REPORT\n", host, ipAddr, packetSize)
	fmt.Printf("%-3s %-20s %-30s %5s %6s %6s %6s %6s %6s\n",
		"TTL", "IP", "HOST", "LOSS%", "SENT", "RECV", "LAST", "AVG", "BEST")

	for i := 1; i <= maxHops; i++ {
		hs := hopsStats[i]
		
		if hs.Addr == nil {
			continue
		}
		
		hostname := "???"
		if resolveIP && hs.Hostname != "" {
			hostname = hs.Hostname
		} else if !resolveIP {
			hostname = hs.Addr.String()
		}
		
		sent, received, last, _, _, avg := hs.Stats()
		
		lastStr := "?"
		if received > 0 {
			lastStr = fmt.Sprintf("%.1f", float64(last)/float64(time.Millisecond))
		}
		
		avgStr := "?"
		if received > 0 {
			avgStr = fmt.Sprintf("%.1f", float64(avg)/float64(time.Millisecond))
		}
		
		bestStr := "?"
		if received > 0 {
			bestStr = fmt.Sprintf("%.1f", float64(hs.Best)/float64(time.Millisecond))
		}
		
		fmt.Printf("%-3d %-20s %-30s %5.1f %6d %6d %6s %6s %6s\n",
			i, hs.Addr.String(), hostname, hs.LossPercentage(),
			sent, received, lastStr, avgStr, bestStr)
	}
}
