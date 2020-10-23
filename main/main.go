package main

import (
	"flag"
	"log"

	"github.com/zhoukk/kftpd"
)

type logNotifier struct{}

func (*logNotifier) FileCreate(user, name string) {
	log.Printf("user: %s create file: %s\n", user, name)
}

func (*logNotifier) FileDelete(user, name string) {
	log.Printf("user: %s delete file: %s\n", user, name)
}

func (*logNotifier) DirCreate(user, name string) {
	log.Printf("user: %s create dir: %s\n", user, name)
}

func (*logNotifier) DirDelete(user, name string) {
	log.Printf("user: %s delete dir: %s\n", user, name)
}

func (*logNotifier) Rename(user, from, to string) {
	log.Printf("user: %s rename: %s to %s\n", user, from, to)
}

func main() {
	var configFile string
	flag.StringVar(&configFile, "c", "kftpd.yaml", "config file")
	flag.Parse()

	config, err := kftpd.LoadFtpdConfig(configFile)
	if err != nil {
		log.Println(err)
		flag.Usage()
		return
	}

	if config.Debug {
		log.Printf("%+v\n", config)
	}

	log.Fatal(kftpd.FtpdServe(config, &logNotifier{}))
}
