package snmp

// ExportValidateOID exposes validateOID to the external _test package. The
// validator is deliberately unexported — callers get it applied automatically
// by Run and RunMany — but its rules are the kind that need direct table tests.
func ExportValidateOID(oid string) error { return validateOID(oid) }
