package main

import (
	"context"
	"flag"
	"github.com/eventure/hide.client.linux/connection"
	"github.com/eventure/hide.client.linux/control"
	"github.com/eventure/hide.client.linux/rest"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// Get the Access-Token
func accessToken( conf *Configuration ) ( err error ) {
	if conf.Rest.AccessTokenPath == "" { log.Println( "AcTo: [ERR] Access-Token must be stored in a file" ); return }
	client := rest.New( conf.Rest )																											// Create the REST client
	if err = client.Init(); err != nil { log.Println( "AcTo: [ERR] REST Client setup failed:", err ); return }
	if !client.HaveAccessToken() {																											// An old Access-Token may be used to obtain a new one, although that process may be done by "connect" too
		if err = client.InteractiveCredentials(); err != nil { log.Println( "AcTo: [ERR] Credential error:", err ); return }				// Try to obtain credentials through the terminal
	}
	ctx, cancel := context.WithTimeout( context.Background(), conf.Rest.RestTimeout )
	defer cancel()
	if err = client.Resolve( ctx ); err != nil { log.Println( "AcTo: [ERR] DNS failed:", err ); return }									// Resolve the REST endpoint
	if _, err = client.GetAccessToken( ctx ); err != nil { log.Println( "AcTo: [ERR] Access-Token request failed:", err ); return }			// Request an Access-Token
	log.Println( "AcTo: Access-Token stored in", conf.Rest.AccessTokenPath )
	return
}

// Fetch and dump filtering categories
func categories( conf *Configuration ) {
	clientConf := conf.Rest
	clientConf.Port = 443
	clientConf.CA = ""
	client := rest.New( clientConf )																										// Create the REST client
	ctx, cancel := context.WithTimeout( context.Background(), conf.Rest.RestTimeout )
	defer cancel()
	if err := client.Init(); err != nil { log.Println( "Main: [ERR] REST Client setup failed:", err ); return }
	if err := client.Resolve( ctx ); err != nil { log.Println( "Main: [ERR] DNS failed:", err ); return }									// Resolve the REST endpoint
	if err := client.FetchCategoryList( ctx ); err != nil { log.Println( "Main: [ERR] GET request failed:", err ); return }					// Get JSON
	return
}

func main() {
	log.SetFlags( 0 )
	var err error
	conf := NewConfiguration()																												// Parse the command line flags and optionally read the configuration file
	if err = conf.Parse(); err != nil { log.Println( "Main: Configuration failed", err.Error() ); return }									// Exit on configuration error
	
	var c *connection.Connection
	var controlServer *control.Server
	
	switch flag.Arg(0) {
		case "conf": conf.Print(); return
		case "jsonconf": conf.PrintJson(); return
		case "service":
			controlServer = control.New( conf.Control, &connection.Config{ Rest: conf.Rest, Wireguard: conf.WireGuard } )
			if err = controlServer.Init(); err != nil { log.Println( "Main: [ERR] Control server initialization failed" ); return }
			go controlServer.Serve()
		case "token", "categories", "connect":
			if len( flag.Arg(1) ) == 0 { flag.Usage(); return }
			conf.Rest.SetHost( flag.Arg(1) )
			switch flag.Arg(0) {
				case "token": _ = accessToken( conf ); return																				// Access-Token
				case "categories": categories( conf ); return																				// Fetch the filtering categories JSON
				case "connect":																												// Connect to the server
					c = connection.New( &connection.Config{ Rest: conf.Rest, Wireguard: conf.WireGuard } )
					if err = c.Init(); err != nil { log.Println( "Main: [ERR] Connect init failed", err.Error() ); return }
					c.NotifySystemd( true )
					c.ScheduleConnect(0)
			}
		default: log.Println( "Main: Unsupported command", flag.Arg(0) ); flag.Usage(); return
	}
	
	signalChannel := make ( chan os.Signal )																								// Signal handling
	signal.Notify( signalChannel, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGUSR1 )
	
	for sig := range signalChannel {
		switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				if c != nil { c.Disconnect(); c.Shutdown() }
				if controlServer != nil { controlServer.Shutdown() }
				return
			case syscall.SIGUSR1:
				if c != nil { c.Disconnect(); c.ScheduleConnect(0) }
			default: return
		}
	}
}
