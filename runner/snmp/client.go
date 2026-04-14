package snmp

import "time"

// Client defines generic SNMP operations.
// Implementations wrap gosnmp or other SNMP libraries.
// Tracing is done via OTel context — no string log collection.
type Client interface {
	Get(ip, community, oid string) (any, error)
	GetMultiple(ip, community string, oids []string) (map[string]any, error)
	GetWithAlias(target Target) (map[string]any, error)
	SetInteger(ip, community, oid string, value int) error
	SetMultiple(ip, community string, values []SetValue) error
}

// Target specifies a bulk SNMP request.
type Target struct {
	IP        string
	Community string
	OIDs      map[string]string // alias → OID
}

// SetValue specifies an SNMP SET operation.
type SetValue struct {
	OID   string
	Type  int // gosnmp ASN.1 BER type (0x02=Integer, 0x04=OctetString)
	Value any
}

// Config holds SNMP connection parameters.
type Config struct {
	Timeout time.Duration
	Retries int
}
