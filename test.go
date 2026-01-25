package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"time"
)

func main() {
	tracker := "tracker.leechers-paradise.org:6969"

	// 1. Resolve UDP Address
	raddr, err := net.ResolveUDPAddr("udp", tracker)
	if err != nil {
		fmt.Printf("Error resolving: %v\n", err)
		return
	}

	// 2. Dial UDP
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		fmt.Printf("Error dialing: %v\n", err)
		return
	}
	defer conn.Close()

	// 3. Construct the 'Connect' Request (BEP 15)
	// Format: Protocol_ID (64-bit) | Action (32-bit) | Transaction_ID (32-bit)
	buf := new(bytes.Buffer)
	protocolID := uint64(0x41727101980) // Magic constant
	action := uint32(0)                 // 0 = Connect
	transID := uint32(rand.Int31())     // Random ID to verify response

	binary.Write(buf, binary.BigEndian, protocolID)
	binary.Write(buf, binary.BigEndian, action)
	binary.Write(buf, binary.BigEndian, transID)

	// 4. Send Packet
	fmt.Printf("Sending connect request to %s...\n", tracker)
	_, err = conn.Write(buf.Bytes())
	if err != nil {
		fmt.Printf("Write error: %v\n", err)
		return
	}

	// 5. Read Response
	// Expect at least 16 bytes: Action (4) | TransID (4) | ConnectionID (8)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response := make([]byte, 16)
	_, _, err = conn.ReadFromUDP(response)

	if err != nil {
		fmt.Println("❌ Timeout or Error: Tracker might be down or blocked.")
		return
	}

	// 6. Verify Response
	respAction := binary.BigEndian.Uint32(response[0:4])
	respTransID := binary.BigEndian.Uint32(response[4:8])

	if respTransID == transID && respAction == 0 {
		fmt.Println("✅ Tracker is ONLINE and responding!")
	} else {
		fmt.Println("⚠️ Received invalid response.")
	}
}
