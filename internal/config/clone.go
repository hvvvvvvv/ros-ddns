package config

// CloneItem returns an independent copy, including the parsed source values and
// optional address-list target, so callers can retain their own configuration.
func CloneItem(c ItemConfig) ItemConfig {
	c.Domains = append([]DomainConfig(nil), c.Domains...)
	c.Source.EUI64MACAddr = append(c.Source.EUI64MACAddr[:0:0], c.Source.EUI64MACAddr...)
	if c.Source.Select.PrefixNet != nil {
		prefix := *c.Source.Select.PrefixNet
		c.Source.Select.PrefixNet = &prefix
	}
	if c.AddressList != nil {
		list := *c.AddressList
		c.AddressList = &list
	}
	return c
}
