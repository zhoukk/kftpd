package main

import (
	"flag"
	"log"

	"github.com/zhoukk/kftpd"
)

func main() {
	var configFile string
	flag.StringVar(&configFile, "c", "kftpd.yaml", "config file")
	flag.Parse()

	config, err := kftpd.NewFtpdConfig(configFile)
	if err != nil {
		log.Println(err)
		flag.Usage()
		return
	}

	if config.Debug {
		log.Printf("%+v\n", config)
	}

	log.Fatal(kftpd.FtpdServe(config))
}
