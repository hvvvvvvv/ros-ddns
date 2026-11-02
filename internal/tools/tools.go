// Package tools pins modules that are used by implementation packages and
// must stay in go.mod even while those packages are being built out.
package tools

import (
	_ "github.com/alibabacloud-go/alidns-20150109/v5/client"
	_ "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	_ "github.com/alibabacloud-go/tea/dara"
	_ "golang.org/x/time/rate"
)
