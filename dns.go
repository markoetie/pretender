package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
)

const (
	dnsPort = 53 // default DNS port

	// DefaultTTL is the time to live specified in responses to name resolution
	// queries of any type.
	dnsDefaultTTL = 60 * time.Second

	// NetBIOS name resolution messages have the type set to 32 which means
	// NIMLOC in the DNS spec. In this case we don't expect actual NIMLOC
	// messages, so we assume type 32 messages to be NetBIOS requests.
	typeNetBios = dns.TypeNIMLOC
)

func createDNSReplyFromRequest(rw dns.ResponseWriter, request *dns.Msg, logger *Logger, config Config) *dns.Msg {
	reply := &dns.Msg{}
	reply.SetReply(request)

	peer, err := toIP(rw.RemoteAddr())
	if err != nil {
		logger.Errorf(err.Error())

		return nil
	}

	for _, q := range request.Question {
		name := normalizedNameFromQuery(q)

		if !shouldRespondToNameResolutionQuery(config, name, peer) {
			logger.IgnoreDNS(name, queryType(q), peer)

			continue
		}

		switch q.Qtype {
		case dns.TypeA:
			if config.RelayIPv4 == nil {
				logger.Infof("ignored AAAA request because no IPv6 relay gateway is configured")

				continue
			}

			reply.Answer = append(reply.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(config.TTL.Seconds())},
				A:   config.RelayIPv4,
			})
		case dns.TypeAAAA:
			if config.RelayIPv6 == nil {
				logger.Infof("ignored AAAA request because no IPv6 relay gateway is configured")

				continue
			}

			reply.Answer = append(reply.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: uint32(config.TTL.Seconds())},
				AAAA: config.RelayIPv6,
			})
		case dns.TypeANY:
			if config.RelayIPv4 != nil {
				reply.Answer = append(reply.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(config.TTL.Seconds())},
					A:   config.RelayIPv4,
				})
			}

			if config.RelayIPv6 != nil {
				reply.Answer = append(reply.Answer, &dns.AAAA{
					Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: uint32(config.TTL.Seconds())},
					AAAA: config.RelayIPv6,
				})
			}
		case typeNetBios:
			reply.CheckingDisabled = false
			reply.Question = nil
			reply.Answer = append(reply.Answer, &dns.NIMLOC{
				Hdr: dns.RR_Header{
					Name: q.Name, Rrtype: dns.TypeNIMLOC, Class: dns.ClassINET,
					Ttl: uint32(config.TTL.Seconds()),
				},
				Locator: encodeNetBIOSLocator(config.RelayIPv4.To4()),
			})
		default:
			logger.Debugf("%s query for name %s from %s on interface %s is unhandled",
				dns.Type(q.Qtype).String(), name, rw.RemoteAddr().String(), rw.LocalAddr().String())

			continue
		}

		logger.Query(name, queryType(q), peer)
	}

	if len(reply.Answer) == 0 {
		return nil
	}

	return reply
}

func toIP(addr net.Addr) (net.IP, error) {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.IP, nil
	case *net.UDPAddr:
		return a.IP, nil
	default:
		return nil, fmt.Errorf("cannot extract IP from %T", addr)
	}
}

func normalizedNameFromQuery(q dns.Question) string {
	if q.Qtype == typeNetBios {
		return decodeNetBIOSHostname(q.Name)
	}

	name := normalizedName(q.Name)

	if name == "" {
		return q.Name
	}

	return name
}

func normalizedName(host string) string {
	return strings.TrimSuffix(strings.TrimSpace(host), ".")
}

func queryType(q dns.Question) string {
	if q.Qtype == typeNetBios {
		return decodeNetBIOSSuffix(q.Name)
	}

	return dnsQueryType(q.Qtype)
}

func dnsQueryType(qtype uint16) string {
	return dns.Type(qtype).String()
}

// DNSHandler creates a dns.HandlerFunc based on the logic in createResponseFromRequest.
func DNSHandler(logger *Logger, config Config) dns.HandlerFunc {
	return func(rw dns.ResponseWriter, request *dns.Msg) {
		reply := createDNSReplyFromRequest(rw, request, logger, config)
		if reply == nil {
			_ = rw.Close() // early abort for TCP connections

			return
		}

		err := rw.WriteMsg(reply)
		if err != nil {
			logger.Errorf("writing response: %v", err)
		}
	}
}

// UDPConnDNSHandler handles requests by creating a response using
// createResponseFromRequest and sends it directly using the underlying UDP
// connection on which the server operates.
func UDPConnDNSHandler(conn net.PacketConn, logger *Logger, config Config) dns.HandlerFunc {
	return func(rw dns.ResponseWriter, request *dns.Msg) {
		reply := createDNSReplyFromRequest(rw, request, logger, config)
		if reply == nil {
			return
		}

		buf, err := reply.Pack()
		if err != nil {
			logger.Errorf("pack message: %v", err)

			return
		}

		_, err = conn.WriteTo(buf, rw.RemoteAddr())
		if err != nil {
			logger.Errorf("write dns response: %v", err)

			return
		}
	}
}

// RunDNSResponder starts a TCP and a UDP DNS server.
func RunDNSResponder(ctx context.Context, logger *Logger, config Config) error {
	errGroup, ctx := errgroup.WithContext(ctx)

	ipv6Addr := net.IPAddr{IP: config.LocalIPv6, Zone: config.Interface.Name}
	fullAddr := net.JoinHostPort(ipv6Addr.String(), strconv.Itoa(dnsPort))

	errGroup.Go(func() error {
		logger.Infof("listening via UDP on %s", fullAddr)

		return runDNSServerWithContext(ctx, &dns.Server{
			Addr:    fullAddr,
			Net:     "udp6",
			Handler: DNSHandler(logger, config),
		})
	})

	errGroup.Go(func() error {
		logger.Infof("listening via TCP on %s", fullAddr)

		return runDNSServerWithContext(ctx, &dns.Server{
			Addr:    fullAddr,
			Net:     "tcp6",
			Handler: DNSHandler(logger, config),
		})
	})

	return errGroup.Wait()
}

func runDNSServerWithContext(ctx context.Context, server *dns.Server) error {
	go func() {
		<-ctx.Done()

		_ = server.Shutdown()
	}()

	err := server.ListenAndServe()
	if err != nil {
		return fmt.Errorf("activate and serve: %w", err)
	}

	return nil
}

// RunDNSHandlerOnUDPConnection runs the DNS handler on an arbitrary UDP
// connection, such that the DNS handling logic can be used for LLMNR, mDNS and
// NetBIOS name resolution.
func RunDNSHandlerOnUDPConnection(ctx context.Context, conn net.PacketConn, logger *Logger, config Config) error {
	server := &dns.Server{
		PacketConn: conn,
		Handler:    UDPConnDNSHandler(conn, logger, config),
	}

	go func() {
		<-ctx.Done()

		_ = server.Shutdown()
	}()

	err := server.ActivateAndServe()
	if err != nil {
		return fmt.Errorf("activate and serve: %w", err)
	}

	return nil
}
