package main

import (
	"context"
	jev "github.com/ByteDeskAI/bytedesk-jev/jevplugin"
	pluginsdk "github.com/ByteDeskAI/bytedesk-remote-gateway-plugin-sdk/v2"
	"log"
)

func main() {
	if err := pluginsdk.ServePlugin(context.Background(), jev.New(), pluginsdk.PluginConfig{}); err != nil {
		log.Fatal(err)
	}
}
