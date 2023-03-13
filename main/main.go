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

	config, err := kftpd.LoadFtpdConfig(configFile)
	if err != nil {
		log.Println(err)
		flag.Usage()
		return
	}

	if config.Debug {
		log.Printf("%+v\n", config)
	}

	// kftpd.UserBeforeLogin(func(user, pass string) bool {
	// 	log.Printf("UserBeforeLogin %s %s\n", user, pass)
	// 	return true
	// })

	// kftpd.UserAfterLogin(func(user string) {
	// 	log.Printf("UserAfterLogin %s\n", user)
	// })

	// kftpd.FileBeforePut(func(user, path string) bool {
	// 	log.Printf("FileBeforePut %s %s\n", user, path)
	// 	return true
	// })

	// kftpd.FileAfterPut(func(user, path string) {
	// 	log.Printf("FileAfterPut %s %s\n", user, path)
	// })

	// kftpd.FileBeforeGet(func(user, path string) bool {
	// 	log.Printf("FileBeforeGet %s %s\n", user, path)
	// 	return true
	// })

	// kftpd.FileAfterGet(func(user, path string) {
	// 	log.Printf("FileAfterGet %s %s\n", user, path)
	// })

	// kftpd.FileBeforeDelete(func(user, path string) bool {
	// 	log.Printf("FileBeforeDelete %s %s\n", user, path)
	// 	return true
	// })

	// kftpd.FileAfterDelete(func(user, path string) {
	// 	log.Printf("FileAfterDelete %s %s\n", user, path)
	// })

	// kftpd.FileBeforeRename(func(user, from, to string) bool {
	// 	log.Printf("FileBeforeRename %s %s %s\n", user, from, to)
	// 	return true
	// })

	// kftpd.FileAfterRename(func(user, from, to string) {
	// 	log.Printf("FileAfterRename %s %s %s\n", user, from, to)
	// })

	log.Fatal(kftpd.FtpdServe(config))
}
