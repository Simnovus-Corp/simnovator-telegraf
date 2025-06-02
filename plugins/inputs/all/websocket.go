//go:build !custom || inputs || inputs.websocket

package all

import _ "github.com/influxdata/telegraf/plugins/inputs/websocket" // register plugin
