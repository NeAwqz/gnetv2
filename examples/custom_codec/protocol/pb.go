package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/panjf2000/gnet"
	"log"
)

// CustomLengthFieldProtocol : custom protocol
// custom protocol header contains Version,ActionType and DataLength fields
// its payload is Data field
type CustomLengthFieldProtocol struct {
	Version    uint16
	ActionType uint16
	DataLength uint32
	Data       []byte
}

// Pack :
// Before sending to the peer, wrap the data using this Pack function.
func Pack(pbVersion, actionType uint16, data []byte) ([]byte, error) {
	result := make([]byte, 0)

	buffer := bytes.NewBuffer(result)

	if err := binary.Write(buffer, binary.BigEndian, pbVersion); err != nil {
		s := fmt.Sprintf("Pack version error , %v", err)
		return nil, errors.New(s)
	}

	if err := binary.Write(buffer, binary.BigEndian, actionType); err != nil {
		s := fmt.Sprintf("Pack type error , %v", err)
		return nil, errors.New(s)
	}
	dataLen := uint32(len(data))
	if err := binary.Write(buffer, binary.BigEndian, dataLen); err != nil {
		s := fmt.Sprintf("Pack datalength error , %v", err)
		return nil, errors.New(s)
	}
	if dataLen > 0 {
		if err := binary.Write(buffer, binary.BigEndian, data); err != nil {
			s := fmt.Sprintf("Pack data error , %v", err)
			return nil, errors.New(s)
		}
	}

	return buffer.Bytes(), nil
}

// Encode ...
func (cc *CustomLengthFieldProtocol) Encode(c gnet.Conn, buf []byte) ([]byte, error) {
	return buf, nil
}

// Decode ...
func (cc *CustomLengthFieldProtocol) Decode(c gnet.Conn) ([]byte, error) {
	// parse header
	headerLen := DefaultHeadLength // uint16+uint16+uint32
	if size, header := c.ReadN(headerLen); size == headerLen {
		byteBuffer := bytes.NewBuffer(header)
		var pbVersion, actionType uint16
		var dataLength uint32
		binary.Read(byteBuffer, binary.BigEndian, &pbVersion)
		binary.Read(byteBuffer, binary.BigEndian, &actionType)
		binary.Read(byteBuffer, binary.BigEndian, &dataLength)
		// to check the protocol version and actionType,
		// reset buffer if the version or actionType is not correct
		if pbVersion != DefaultProtocolVersion || isCorrectAction(actionType) == false {
			c.ResetBuffer()
			log.Println("not normal protocol:", pbVersion, DefaultProtocolVersion, actionType, dataLength)
			return nil, errors.New("not normal protocol")
		}
		// parse payload
		dataLen := int(dataLength) //max int32 can contain 210MB payload
		protocolLen := headerLen + dataLen
		if dataSize, data := c.ReadN(protocolLen); dataSize == protocolLen {
			c.ShiftN(protocolLen)
			// log.Println("parse success:", data, dataSize)

			// return the payload of the data
			return data[headerLen:], nil
		}
		// log.Println("not enough payload data:", dataLen, protocolLen, dataSize)
		return nil, errors.New("not enough payload data")

	}
	// log.Println("not enough header data:", size)
	return nil, errors.New("not enough header data")
}

// default custom protocol const
const (
	DefaultHeadLength = 8

	DefaultProtocolVersion = 0x8001 // test protocol version

	ActionPing = 0x0001 // ping
	ActionPong = 0x0002 // pong
	ActionData = 0x00F0 // business
)

func isCorrectAction(actionType uint16) bool {
	switch actionType {
	case ActionPing, ActionPong, ActionData:
		return true
	default:
		return false
	}
}
