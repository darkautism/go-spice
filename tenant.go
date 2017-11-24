package spice

import (
	"bufio"
	"io"
	"net"

	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"

	"fmt"

	"github.com/jsimonetti/go-spice/red"
)

type tenantHandshake struct {
	proxy *Proxy

	done bool

	tenantAuthMethod red.AuthMethod
	privateKey       *rsa.PrivateKey

	channelID   uint8
	channelType red.ChannelType
	sessionID   uint32

	otp         string // one time password
	destination string // compute address
}

func (c *tenantHandshake) Done() bool {
	return c.done
}

func (c *tenantHandshake) clientLinkStage(tenant net.Conn) (net.Conn, error) {
	bufConn := bufio.NewReader(tenant)

	// Handle first Tenant Link Message
	if err := c.clientLinkMessage(bufConn, tenant); err != nil {
		return nil, err
	}

	c.otp = c.proxy.sessionTable.OTP(c.sessionID)

	// Handle 2nd Tenant auth method select
	if err := c.clientAuthMethod(bufConn, tenant); err != nil {
		return nil, err
	}

	// Do compute handshake
	handShake := &computeHandshake{
		proxy:       c.proxy,
		channelType: c.channelType,
		channelID:   c.channelID,
		sessionID:   c.sessionID,
		tenant:      tenant,
	}

	// Lookup destination in proxy.sessionTable
	if c.proxy.sessionTable.Lookup(c.sessionID) {
		var err error
		c.destination, err = c.proxy.sessionTable.Connect(c.sessionID)
		if err != nil {
			return nil, err
		}
	}

	for !handShake.Done() {
		if err := handShake.clientLinkStage(c.destination); err != nil {
			c.proxy.log.WithError(err).Error("compute handshake error")
			return nil, err
		}
	}

	c.sessionID = handShake.sessionID
	c.proxy.sessionTable.Add(c.sessionID, c.destination, c.otp)
	c.done = true

	return handShake.compute, nil
}

func (c *tenantHandshake) clientAuthMethod(in io.Reader, conn net.Conn) error {
	var err error
	b := make([]byte, 4)

	if _, err = in.Read(b); err != nil {
		c.proxy.log.WithError(err).Error("error reading client AuthMethod")
		return err
	}

	c.tenantAuthMethod = red.AuthMethod(b[0])

	var auth Authenticator
	var ok bool

	if auth, ok = c.proxy.authenticator[c.tenantAuthMethod]; !ok {
		if err := sendServerTicket(red.ErrorPermissionDenied, conn); err != nil {
			c.proxy.log.WithError(err).Warn("send ticket")
		}
		return fmt.Errorf("unavailable auth method %s", c.tenantAuthMethod)
	}

	authCtx := &AuthContext{tenant: conn, privateKey: c.privateKey, otp: c.otp, address: c.destination}

	result, destination, err := auth.Next(authCtx)
	if err != nil {
		c.proxy.log.WithError(err).Error("authentication error")
		return err
	}

	c.otp = authCtx.otp
	c.destination = destination

	if !result {
		if err := sendServerTicket(red.ErrorPermissionDenied, conn); err != nil {
			c.proxy.log.WithError(err).Warn("send ticket")
			return err
		}
		return fmt.Errorf("authentication failed")
	}

	if err := sendServerTicket(red.ErrorOk, conn); err != nil {
		return err
	}

	return nil
}

func (c *tenantHandshake) clientLinkMessage(in io.Reader, out io.Writer) error {
	var err error
	var b []byte

	if b, err = readLinkPacket(in); err != nil {
		c.proxy.log.WithError(err).Error("error reading link packet")
		return err
	}

	linkMessage := &red.ClientLinkMessage{}
	if err := linkMessage.UnmarshalBinary(b); err != nil {
		return err
	}

	c.channelType = linkMessage.ChannelType
	c.channelID = linkMessage.ChannelID
	c.sessionID = linkMessage.SessionID

	if err := c.sendServerLinkMessage(out); err != nil {
		return err
	}

	return nil
}

func (c *tenantHandshake) sendServerLinkMessage(writer io.Writer) error {
	pubkey, err := c.getPubkey()
	if err != nil {
		return err
	}

	reply := red.ServerLinkMessage{
		Error:               red.ErrorOk,
		PubKey:              pubkey,
		CommonCaps:          1,
		ChannelCaps:         1,
		CommonCapabilities:  []uint32{0x0b},
		ChannelCapabilities: []uint32{0x09},
	}
	b, err := reply.MarshalBinary()
	if err != nil {
		return err
	}

	header := red.LinkHeader{
		Size: reply.CapsOffset + 8,
	}
	b2, err := header.MarshalBinary()
	if err != nil {
		return err
	}

	data := append(b2, b...)

	_, err = writer.Write(data)
	if err != nil {
		return err
	}

	return nil
}

func (c *tenantHandshake) getPubkey() (ret [red.TicketPubkeyBytes]byte, err error) {
	rng := rand.Reader
	key, err := rsa.GenerateKey(rng, 1024)
	if err != nil {
		return ret, err
	}
	c.privateKey = key

	cert, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		c.proxy.log.WithError(err).Error("rsa key parse error")
		return ret, err
	}

	copy(ret[:], cert[:])
	return ret, nil
}

func readLinkPacket(conn io.Reader) ([]byte, error) {
	headerBytes := make([]byte, 16)

	if _, err := conn.Read(headerBytes); err != nil {
		return nil, err
	}

	header := &red.LinkHeader{}
	if err := header.UnmarshalBinary(headerBytes); err != nil {
		return nil, err
	}

	var messageBytes []byte
	var n int
	var err error
	pending := int(header.Size)

	for pending > 0 {
		bytes := make([]byte, header.Size)
		if n, err = conn.Read(bytes); err != nil {
			return nil, err
		}
		pending = pending - n
		messageBytes = append(messageBytes, bytes[:n]...)
	}

	return messageBytes[:int(header.Size)], nil
}

func sendServerTicket(result red.ErrorCode, writer io.Writer) error {
	msg := red.ServerTicket{
		Result: result,
	}

	b, err := msg.MarshalBinary()
	if err != nil {
		return err
	}

	if _, err := writer.Write(b); err != nil {
		return err
	}

	return nil
}
