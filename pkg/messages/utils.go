package messages

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"net"
)

func readMsgShared(c net.Conn) (buffer []byte, err error) {

	var sz int64
	err = binary.Read(c, binary.LittleEndian, &sz)
	if err != nil {
		return
	}

	buffer = make([]byte, sz)
	n, err := c.Read(buffer)

	if err != nil {
		return
	}

	if int64(n) != sz {
		err = errors.New(fmt.Sprintf("Expected to read %d bytes, but only read %d", sz, n))
		return
	}

	return
}

func ReadMsg(c net.Conn) (msg proto.Message, err error) {
	buffer, err := readMsgShared(c)
	if err != nil {
		return
	}
	var o proto.Message
	err = proto.Unmarshal(buffer, o)
	if err != nil {
		return
	}
	return o, nil
}

func ReadMsgInto(c net.Conn, msg proto.Message) (err error) {
	buffer, err := readMsgShared(c)
	if err != nil {
		return
	}
	err = proto.Unmarshal(buffer, msg)
	if err != nil {
		return
	}
	return err
}

func WriteMsg(c net.Conn, msg proto.Message) (err error) {
	buffer, err := proto.Marshal(msg)
	if err != nil {
		return
	}

	err = binary.Write(c, binary.LittleEndian, int64(len(buffer)))

	if err != nil {
		return
	}

	if _, err = c.Write(buffer); err != nil {
		return
	}

	return nil
}
