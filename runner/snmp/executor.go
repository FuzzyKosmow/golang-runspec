package snmp

import (
	"fmt"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// goSNMPExecutor implements Client using the gosnmp library.
type goSNMPExecutor struct {
	cfg Config
}

// NewExecutor creates an SNMP client backed by gosnmp.
func NewExecutor(cfg Config) Client {
	return &goSNMPExecutor{cfg: cfg}
}

func (e *goSNMPExecutor) Get(ip, community, oid string) (any, error) {
	resultMap, err := e.GetMultiple(ip, community, []string{oid})
	if err != nil {
		return nil, err
	}
	return resultMap[oid], nil
}

func (e *goSNMPExecutor) GetMultiple(ip, community string, oids []string) (map[string]any, error) {
	if len(oids) == 0 {
		return make(map[string]any), nil
	}

	params := &gosnmp.GoSNMP{
		Target:    ip,
		Port:      161,
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   e.cfg.Timeout,
		Retries:   e.cfg.Retries,
	}

	if err := params.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect %s: %w", ip, err)
	}
	defer params.Conn.Close()

	result, err := params.Get(oids)
	if err != nil {
		return nil, fmt.Errorf("snmp get %v on %s: %w", oids, ip, err)
	}
	if result.Error != gosnmp.NoError {
		return nil, fmt.Errorf("snmp device error %s: %s", ip, result.Error)
	}

	resultsMap := make(map[string]any)
	for _, pdu := range result.Variables {
		normalizedName := pdu.Name
		if len(normalizedName) > 0 && normalizedName[0] == '.' {
			normalizedName = normalizedName[1:]
		}

		var finalValue any = pdu.Value
		if pdu.Type == gosnmp.OctetString {
			if bytes, ok := pdu.Value.([]byte); ok {
				if isPrintableASCII(bytes) {
					finalValue = strings.TrimSpace(string(bytes))
				}
			}
		}

		resultsMap[pdu.Name] = finalValue
		resultsMap[normalizedName] = finalValue
	}

	return resultsMap, nil
}

func (e *goSNMPExecutor) GetWithAlias(target Target) (map[string]any, error) {
	oids := make([]string, 0, len(target.OIDs))
	for _, oid := range target.OIDs {
		oids = append(oids, oid)
	}

	rawResults, err := e.GetMultiple(target.IP, target.Community, oids)
	if err != nil {
		return nil, err
	}

	aliasResults := make(map[string]any)
	for alias, requestedOID := range target.OIDs {
		if val, ok := rawResults[requestedOID]; ok {
			aliasResults[alias] = val
			continue
		}
		if val, ok := rawResults["."+requestedOID]; ok {
			aliasResults[alias] = val
			continue
		}
		if len(requestedOID) > 0 && requestedOID[0] == '.' {
			if val, ok := rawResults[requestedOID[1:]]; ok {
				aliasResults[alias] = val
				continue
			}
		}
	}

	return aliasResults, nil
}

func (e *goSNMPExecutor) SetInteger(ip, community, oid string, value int) error {
	params := &gosnmp.GoSNMP{
		Target:    ip,
		Port:      161,
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   e.cfg.Timeout,
		Retries:   e.cfg.Retries,
	}

	if err := params.Connect(); err != nil {
		return fmt.Errorf("snmp connect %s: %w", ip, err)
	}
	defer params.Conn.Close()

	result, err := params.Set([]gosnmp.SnmpPDU{{Name: oid, Type: gosnmp.Integer, Value: value}})
	if err != nil {
		return fmt.Errorf("snmp set %s on %s: %w", oid, ip, err)
	}
	if result.Error != gosnmp.NoError {
		return fmt.Errorf("snmp device error %s: %s", ip, result.Error)
	}
	return nil
}

func (e *goSNMPExecutor) SetMultiple(ip, community string, values []SetValue) error {
	if len(values) == 0 {
		return nil
	}

	params := &gosnmp.GoSNMP{
		Target:    ip,
		Port:      161,
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   e.cfg.Timeout,
		Retries:   e.cfg.Retries,
	}

	if err := params.Connect(); err != nil {
		return fmt.Errorf("snmp connect %s: %w", ip, err)
	}
	defer params.Conn.Close()

	var pdus []gosnmp.SnmpPDU
	for _, val := range values {
		pdus = append(pdus, gosnmp.SnmpPDU{
			Name:  val.OID,
			Type:  gosnmp.Asn1BER(val.Type),
			Value: val.Value,
		})
	}

	result, err := params.Set(pdus)
	if err != nil {
		return fmt.Errorf("snmp set multiple on %s: %w", ip, err)
	}
	if result.Error != gosnmp.NoError {
		return fmt.Errorf("snmp device error %s: %s", ip, result.Error)
	}
	return nil
}

func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}
