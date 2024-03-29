package kftpd

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gopkg.in/yaml.v3"
)

// FtpdConfig - ftpd configure
type FtpdConfig struct {
	Bind    string `yaml:"Bind,omitempty"`
	Driver  string `yaml:"Driver,omitempty"`
	HomeDir bool   `yaml:"HomeDir,omitempty"`
	Debug   bool   `yaml:"Debug,omitempty"`

	Pasv struct {
		Enable        bool   `yaml:"Enable,omitempty"`
		IP            string `yaml:"IP,omitempty"`
		PortStart     int    `yaml:"PortStart,omitempty"`
		PortEnd       int    `yaml:"PortEnd,omitempty"`
		ListenTimeout int    `yaml:"ListenTimeout,omitempty"`
	} `yaml:"Pasv,omitempty"`

	Port struct {
		Enable         bool `yaml:"Enable,omitempty"`
		ConnectTimeout int  `yaml:"ConnectTimeout,omitempty"`
	} `yaml:"Port,omitempty"`

	FileDriver struct {
		BaseDir string `yaml:"BaseDir,omitempty"`
	} `yaml:"FileDriver,omitempty"`

	MinioDriver struct {
		Endpoint        string `yaml:"Endpoint,omitempty"`
		AccessKeyID     string `yaml:"AccessKeyID,omitempty"`
		SecretAccessKey string `yaml:"SecretAccessKey,omitempty"`
		UseSSL          bool   `yaml:"UseSSL,omitempty"`
		Bucket          string `yaml:"Bucket,omitempty"`
	} `yaml:"MinioDriver,omitempty"`

	AuthTLS struct {
		Enable   bool   `yaml:"Enable,omitempty"`
		CertFile string `yaml:"CertFile,omitempty"`
		KeyFile  string `yaml:"KeyFile,omitempty"`
	} `yaml:"AuthTLS,omitempty"`

	Users map[string]string `yaml:"Users,omitempty"`
}

// DriverFactory - new a driver
type DriverFactory interface {
	NewDriver(string) (Driver, error)
}

// FileInfo - ftp file information
type FileInfo interface {
	os.FileInfo
}

// Driver - file driver interface
type Driver interface {
	Stat(string) (FileInfo, error)

	Chtimes(string, time.Time, time.Time) error

	DeleteDir(string) error

	DeleteFile(string) error

	Rename(string, string) error

	MakeDir(string) error

	ListDir(string, func(FileInfo) error) error

	GetFile(string, int64) (int64, io.ReadCloser, error)

	PutFile(string, int64, io.Reader) (int64, error)
}

// MinioDriverFactory - minio driver factory
type MinioDriverFactory struct {
	endpoint        string
	accessKeyID     string
	secretAccessKey string
	useSSL          bool
	bucket          string
}

// NewMinioDriverFactory return a minio driver factory
func NewMinioDriverFactory(endpoint, accessKeyID, secretAccessKey, bucket string, useSSL bool) DriverFactory {
	return &MinioDriverFactory{
		endpoint:        endpoint,
		accessKeyID:     accessKeyID,
		secretAccessKey: secretAccessKey,
		useSSL:          useSSL,
		bucket:          bucket,
	}
}

// MinioFileInfo - minio file information
type MinioFileInfo struct {
	name   string
	object minio.ObjectInfo
	isDir  bool
}

// Name return minio file name
func (m *MinioFileInfo) Name() string {
	return m.name
}

// Size return minio file size
func (m *MinioFileInfo) Size() int64 {
	if m.isDir {
		return 4096
	}
	return m.object.Size
}

// Mode return minio file mode
func (m *MinioFileInfo) Mode() os.FileMode {
	if m.isDir {
		return os.ModePerm | os.ModeDir
	}
	return os.ModePerm
}

// ModTime return minio file modify time
func (m *MinioFileInfo) ModTime() time.Time {
	if m.isDir {
		return time.Now()
	}
	return m.object.LastModified
}

// IsDir return minio path is dir
func (m *MinioFileInfo) IsDir() bool {
	return m.isDir
}

// Sys return minio file system information, not implemented.
func (m *MinioFileInfo) Sys() interface{} {
	return nil
}

// MinioDriver - minio driver
type MinioDriver struct {
	client *minio.Client
	bucket string
	user   string
}

// NewDriver return a minio driver
func (factory *MinioDriverFactory) NewDriver(user string) (Driver, error) {
	client, err := minio.New(factory.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(factory.accessKeyID, factory.secretAccessKey, ""),
		Secure: factory.useSSL,
	})
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	err = client.MakeBucket(ctx, factory.bucket, minio.MakeBucketOptions{ObjectLocking: false})
	if err != nil {
		exists, errBucketExists := client.BucketExists(ctx, factory.bucket)
		if !exists || errBucketExists != nil {
			return nil, err
		}
	}

	return &MinioDriver{client, factory.bucket, user}, nil
}

// miniopath return file path joined with user
func (driver *MinioDriver) miniopath(path string) string {
	return filepath.Join(driver.user, path)
}

// miniodir return dir path joined with user
func (driver *MinioDriver) miniodir(path string) string {
	dir := filepath.Join(driver.user, path)
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	if dir != "/" && strings.HasPrefix(dir, "/") {
		dir = strings.TrimPrefix(dir, "/")
	}
	return dir
}

// Stat return file information
func (driver *MinioDriver) Stat(path string) (FileInfo, error) {
	if path == "/" {
		return &MinioFileInfo{
			name:  "/",
			isDir: true,
		}, nil
	}

	rpath := driver.miniopath(path)
	object, err := driver.client.StatObject(context.Background(), driver.bucket, rpath, minio.StatObjectOptions{})
	if err != nil {
		return &MinioFileInfo{
			name:  rpath,
			isDir: true,
		}, nil
	}
	return &MinioFileInfo{
		name:   strings.TrimSuffix(strings.TrimPrefix(object.Key, rpath), "/"),
		object: object,
		isDir:  strings.HasSuffix(object.Key, "/"),
	}, nil
}

// Chtimes change file modify time
func (driver *MinioDriver) Chtimes(path string, atime time.Time, mtime time.Time) error {
	return errors.New("not implemented")
}

// DeleteDir delete dir in minio
func (driver *MinioDriver) DeleteDir(path string) error {
	rpath := driver.miniodir(path)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	objectCh := driver.client.ListObjects(ctx, driver.bucket, minio.ListObjectsOptions{
		Prefix:    rpath,
		Recursive: false,
	})
	for rErr := range driver.client.RemoveObjects(ctx, driver.bucket, objectCh, minio.RemoveObjectsOptions{
		GovernanceBypass: true,
	}) {
		return rErr.Err
	}
	return driver.client.RemoveObject(ctx, driver.bucket, rpath, minio.RemoveObjectOptions{})
}

// DeleteFile delete file in minio
func (driver *MinioDriver) DeleteFile(path string) error {
	rpath := driver.miniopath(path)
	return driver.client.RemoveObject(context.Background(), driver.bucket, rpath, minio.RemoveObjectOptions{})
}

// Rename rename file or dir in minio
func (driver *MinioDriver) Rename(from string, to string) error {
	fpath := driver.miniopath(from)
	tpath := driver.miniopath(to)
	ctx := context.Background()

	rename := func(from, to string) error {
		_, err := driver.client.CopyObject(ctx, minio.CopyDestOptions{
			Bucket: driver.bucket,
			Object: to,
		}, minio.CopySrcOptions{
			Bucket: driver.bucket,
			Object: from,
		})
		if err == nil {
			err = driver.client.RemoveObject(ctx, driver.bucket, fpath, minio.RemoveObjectOptions{})
		}
		return err
	}

	err := rename(fpath, tpath)
	if err != nil {
		fpath += "/"
		tpath += "/"
		err = rename(fpath, tpath)
	}

	return err
}

// MakeDir make dir in minio
func (driver *MinioDriver) MakeDir(path string) error {
	rpath := driver.miniodir(path)
	_, err := driver.client.PutObject(context.Background(), driver.bucket, rpath, nil, 0, minio.PutObjectOptions{})
	return err
}

// GetFile return file size, file reader in minio
func (driver *MinioDriver) GetFile(path string, offset int64) (int64, io.ReadCloser, error) {
	rpath := driver.miniopath(path)

	object, err := driver.client.GetObject(context.Background(), driver.bucket, rpath, minio.GetObjectOptions{})
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil && object != nil {
			object.Close()
		}
	}()
	info, err := object.Stat()
	if err != nil {
		return 0, nil, err
	}
	if offset > 0 {
		_, err = object.Seek(offset, io.SeekStart)
		if err != nil {
			return 0, nil, err
		}
	}

	return info.Size - offset, object, nil
}

// PutFile put a file to minio, support append with offset.
func (driver *MinioDriver) PutFile(path string, offset int64, reader io.Reader) (int64, error) {
	rpath := driver.miniopath(path)

	if offset == 0 {
		info, err := driver.client.PutObject(context.Background(), driver.bucket, rpath, reader, -1, minio.PutObjectOptions{})
		if err != nil {
			return 0, err
		}
		return info.Size, nil
	}

	ctx := context.Background()

	tmppath := rpath + ".tmp"

	defer func() {
		driver.client.RemoveObject(ctx, driver.bucket, tmppath, minio.RemoveObjectOptions{})
	}()

	_, err := driver.client.PutObject(ctx, driver.bucket, tmppath, reader, -1, minio.PutObjectOptions{})
	if err != nil {
		return 0, err
	}
	info, err := driver.client.ComposeObject(ctx,
		minio.CopyDestOptions{Bucket: driver.bucket, Object: rpath},
		minio.CopySrcOptions{Bucket: driver.bucket, Object: rpath},
		minio.CopySrcOptions{Bucket: driver.bucket, Object: tmppath})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// ListDir return file list from dir in minio
func (driver *MinioDriver) ListDir(path string, callback func(FileInfo) error) error {
	rpath := driver.miniodir(path)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	objectCh := driver.client.ListObjects(ctx, driver.bucket, minio.ListObjectsOptions{
		Prefix:    rpath,
		Recursive: false,
	})
	for object := range objectCh {
		if object.Err != nil {
			return object.Err
		}
		if object.Key == rpath {
			continue
		}
		info := &MinioFileInfo{
			name:   strings.TrimSuffix(strings.TrimPrefix(object.Key, rpath), "/"),
			object: object,
			isDir:  strings.HasSuffix(object.Key, "/"),
		}
		err := callback(info)
		if err != nil {
			return err
		}
	}
	return nil
}

// FileDriverFactory - file based driver factory
type FileDriverFactory struct {
	root string
}

// NewFileDriverFactory return a file based driver factory
func NewFileDriverFactory(root string) DriverFactory {
	_, err := os.Lstat(root)
	if os.IsNotExist(err) {
		os.MkdirAll(root, os.ModePerm)
	} else if err != nil {
		log.Printf("NewFileDriverFactory fail, err: %v\n", err)
		os.Exit(-1)
	}
	return &FileDriverFactory{
		root: root,
	}
}

// FileDriver - file based driver
type FileDriver struct {
	root string
}

// NewDriver return a file based driver
func (factory *FileDriverFactory) NewDriver(user string) (Driver, error) {
	var err error
	root, err := filepath.Abs(filepath.Join(factory.root, user))
	if err != nil {
		return nil, err
	}
	_, err = os.Lstat(root)
	if os.IsNotExist(err) {
		os.MkdirAll(root, os.ModePerm)
	} else if err != nil {
		return nil, err
	}
	return &FileDriver{root}, nil
}

// abspath return abs path joined with driver root path
func (driver *FileDriver) abspath(path string) string {
	return filepath.Join(driver.root, path)
}

// Stat return file information
func (driver *FileDriver) Stat(path string) (FileInfo, error) {
	return os.Lstat(driver.abspath(path))
}

// Chtimes change file modify time
func (driver *FileDriver) Chtimes(path string, atime time.Time, mtime time.Time) error {
	return os.Chtimes(driver.abspath(path), atime, mtime)
}

// DeleteDir delete a dir
func (driver *FileDriver) DeleteDir(path string) error {
	rpath := driver.abspath(path)
	fi, err := os.Lstat(rpath)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return os.RemoveAll(rpath)
	}
	return errors.New("not a directory")
}

// DeleteFile delete a file
func (driver *FileDriver) DeleteFile(path string) error {
	rpath := driver.abspath(path)
	fi, err := os.Lstat(rpath)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return os.Remove(rpath)
	}
	return errors.New("not a file")
}

// Rename rename a file or dir
func (driver *FileDriver) Rename(from string, to string) error {
	frpath := driver.abspath(from)
	trpath := driver.abspath(to)
	return os.Rename(frpath, trpath)
}

// MakeDir make a dir
func (driver *FileDriver) MakeDir(path string) error {
	return os.MkdirAll(driver.abspath(path), os.ModePerm)
}

// GetFile return file size, file reader
func (driver *FileDriver) GetFile(path string, offset int64) (int64, io.ReadCloser, error) {
	f, err := os.Open(driver.abspath(path))
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil && f != nil {
			f.Close()
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	if offset > 0 {
		_, err = f.Seek(offset, io.SeekStart)
		if err != nil {
			return 0, nil, err
		}
	}

	return fi.Size() - offset, f, nil
}

// PutFile put a file, support append with offset.
func (driver *FileDriver) PutFile(path string, offset int64, reader io.Reader) (int64, error) {
	rpath := driver.abspath(path)

	fi, err := os.Lstat(rpath)
	if err == nil && fi.IsDir() {
		return 0, errors.New("directory already exist")
	}

	ff := os.O_WRONLY
	if offset > 0 {
		ff |= os.O_APPEND
	} else {
		ff |= os.O_CREATE | os.O_TRUNC
	}

	f, err := os.OpenFile(rpath, ff, 0666)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if offset > 0 {
		_, err = f.Seek(offset, io.SeekStart)
		if err != nil {
			return 0, err
		}
	}

	return io.Copy(f, reader)
}

// ListDir return file list in dir
func (driver *FileDriver) ListDir(path string, callback func(FileInfo) error) error {
	rpath := driver.abspath(path)
	return filepath.Walk(rpath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(rpath, path)
		if name == info.Name() {
			err = callback(info)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
}

// FtpdHandler - ftpd handler
type FtpdHandler struct {
	UserBeforeLogin func(string, string) bool
	UserAfterLogin  func(string)

	ClientBeforePasv func(string) bool
	ClientBeforePort func(string) bool

	FileBeforePut func(string, string) bool
	FileAfterPut  func(string, string)

	FileBeforeGet func(string, string) bool
	FileAfterGet  func(string, string)

	FileBeforeDelete func(string, string) bool
	FileAfterDelete  func(string, string)

	FileBeforeRename func(string, string, string) bool
	FileAfterRename  func(string, string, string)
}

// ftpHandler - ftpd global handler
var ftpHandler FtpdHandler

// FtpConn - ftp session
type FtpConn struct {
	id        int
	arg       string
	user      string
	path      string
	mode      string
	clnt      string
	rename    string
	authd     bool
	tls       bool
	offset    int64
	config    *FtpdConfig
	tlsConfig *tls.Config
	factory   DriverFactory
	driver    Driver
	ctrlConn  net.Conn
	dataConn  net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	lock      sync.Mutex
	pasvPort  int
	notify    chan int
}

// FtpCmd - ftp command handler
type FtpCmd struct {
	Fn   func(*FtpConn) error
	Auth bool
}

var cmdMap = map[string]FtpCmd{
	// Authentication
	"USER": {(*FtpConn).handleUSER, false},
	"PASS": {(*FtpConn).handlePASS, false},

	// TLS handling
	"AUTH": {(*FtpConn).handleAUTH, false},
	"PROT": {(*FtpConn).handlePROT, false},
	"PBSZ": {(*FtpConn).handlePBSZ, false},

	// Misc
	"CLNT": {(*FtpConn).handleCLNT, false},
	"FEAT": {(*FtpConn).handleFEAT, false},
	"SYST": {(*FtpConn).handleSYST, false},
	"NOOP": {(*FtpConn).handleNOOP, false},
	"OPTS": {(*FtpConn).handleOPTS, false},
	"QUIT": {(*FtpConn).handleQUIT, false},

	// File access
	"SIZE": {(*FtpConn).handleSIZE, true},
	"STAT": {(*FtpConn).handleSTAT, true},
	"MDTM": {(*FtpConn).handleMDTM, true},
	"MFMT": {(*FtpConn).handleMFMT, true},
	"RETR": {(*FtpConn).handleRETR, true},
	"STOR": {(*FtpConn).handleSTOR, true},
	"APPE": {(*FtpConn).handleAPPE, true},
	"DELE": {(*FtpConn).handleDELE, true},
	"RNFR": {(*FtpConn).handleRNFR, true},
	"RNTO": {(*FtpConn).handleRNTO, true},
	"ALLO": {(*FtpConn).handleALLO, true},
	"REST": {(*FtpConn).handleREST, true},
	"SITE": {(*FtpConn).handleSITE, true},

	// Directory handling
	"CWD":  {(*FtpConn).handleCWD, true},
	"PWD":  {(*FtpConn).handlePWD, true},
	"CDUP": {(*FtpConn).handleCDUP, true},
	"NLST": {(*FtpConn).handleNLST, true},
	"LIST": {(*FtpConn).handleLIST, true},
	"MLSD": {(*FtpConn).handleMLSD, true},
	"MLST": {(*FtpConn).handleMLST, true},
	"MKD":  {(*FtpConn).handleMKD, true},
	"XMKD": {(*FtpConn).handleMKD, true},
	"RMD":  {(*FtpConn).handleRMD, true},
	"XRMD": {(*FtpConn).handleRMD, true},

	// Connection handling
	"TYPE": {(*FtpConn).handleTYPE, true},
	"PASV": {(*FtpConn).handlePASV, true},
	"EPSV": {(*FtpConn).handlePASV, true},
	"PORT": {(*FtpConn).handlePORT, true},
}

func (fc *FtpConn) handleUSER() error {
	fc.authd = false
	fc.user = fc.arg
	fc.Send(331, "Please specify the password.")
	return nil
}

func (fc *FtpConn) handlePASS() error {
	loginOk := false
	if ftpHandler.UserBeforeLogin != nil {
		loginOk = ftpHandler.UserBeforeLogin(fc.user, fc.arg)
	} else {
		pwd, ok := fc.config.Users[fc.user]
		if ok && pwd == fc.arg {
			loginOk = true
		}
	}
	if loginOk {
		home := ""
		if fc.config.HomeDir {
			home = fc.user
		}
		driver, err := fc.factory.NewDriver(home)
		if err != nil {
			fc.Close()
			return err
		}
		fc.driver = driver
		fc.authd = true
		fc.Send(230, "Login successful.")
		if ftpHandler.UserAfterLogin != nil {
			ftpHandler.UserAfterLogin(fc.user)
		}
		return nil
	}
	fc.Send(530, "Login incorrect.")
	return nil
}

func (fc *FtpConn) handleAUTH() error {
	if !fc.config.AuthTLS.Enable {
		fc.Send(550, "Auth not enable.")
		return nil
	}
	if !fc.tls && (fc.arg == "TLS" || fc.arg == "SSL") {
		conn := tls.Server(fc.ctrlConn, fc.tlsConfig)
		err := conn.Handshake()
		if err != nil {
			fc.Send(421, fmt.Sprintf("Negotiation failed: %s", err.Error()))
			return err
		}
		fc.ctrlConn = conn
		fc.reader = bufio.NewReader(conn)
		fc.writer = bufio.NewWriter(conn)
		fc.tls = true
		fc.Send(234, "Proceed with negotiation.")
		return nil
	}
	fc.Send(504, "Unknown AUTH type.")
	return nil
}

func (fc *FtpConn) handlePROT() error {
	if fc.tls {
		if fc.arg == "P" {
			fc.Send(200, "OK")
		} else {
			fc.Send(536, "Only P level is supported.")
		}
		return nil
	}
	fc.Send(550, "Permission denied.")
	return nil
}

func (fc *FtpConn) handlePBSZ() error {
	if fc.tls && fc.arg == "0" {
		fc.Send(200, "OK")
		return nil
	}
	fc.Send(550, "Permission denied.")
	return nil
}

func (fc *FtpConn) handleCLNT() error {
	fc.clnt = fc.arg
	fc.Send(200, "Noted.")
	return nil
}

func (fc *FtpConn) handleFEAT() error {
	feats := []string{"CLNT", "EPSV", "MDTM", "MFMT", "MLSD", "MLST", "PASV", "PBSZ", "PROT", "REST STREAM", "SIZE", "TVFS", "UTF8"}
	if fc.config.AuthTLS.Enable {
		feats = append([]string{"AUTH TLS"}, feats...)
	}
	for i, feat := range feats {
		feats[i] = " " + feat
	}
	fc.SendMulti(211, "Features:", strings.Join(feats, "\r\n"), "End")
	return nil
}

func (fc *FtpConn) handleSYST() error {
	fc.Send(215, "UNIX Type: L8")
	return nil
}

func (fc *FtpConn) handleNOOP() error {
	fc.Send(200, "NOOP ok.")
	return nil
}

func (fc *FtpConn) handleOPTS() error {
	if strings.ToUpper(fc.arg) == "UTF8 ON" {
		fc.Send(200, "Always in UTF8 mode.")
		return nil
	}
	fc.Send(501, "Option not understood.")
	return nil
}

func (fc *FtpConn) handleQUIT() error {
	fc.Send(221, "Goodbye.")
	fc.Close()
	return nil
}

func (fc *FtpConn) handleSIZE() error {
	path := fc.buildPath(fc.arg)
	fi, err := fc.driver.Stat(path)
	if err != nil {
		fc.Send(550, "Could not get file size.")
		return err
	}
	fc.Send(213, fmt.Sprintf("%d", fi.Size()))
	return nil
}

func (fc *FtpConn) handleSTAT() error {
	if fc.arg == "" {
		status := []string{
			fmt.Sprintf("Connected to %s", fc.ctrlConn.LocalAddr().(*net.TCPAddr).IP.String()),
			fmt.Sprintf("Logged in as %s", fc.user),
			fmt.Sprintf("TYPE: %s", fc.mode),
			"KFtpd",
		}
		for i, stat := range status {
			status[i] = "     " + stat
		}
		fc.SendMulti(211, "FTP server status:", strings.Join(status, "\r\n"), "End of status")
		return nil
	}

	var status []string
	path := fc.buildPath(fc.arg)
	fi, err := fc.driver.Stat(path)
	if err == nil {
		if fi.IsDir() {
			fc.driver.ListDir(path, func(fi FileInfo) error {
				status = append(status, fc.fileStat(fi))
				return nil
			})
		} else {
			status = append(status, fc.fileStat(fi))
		}
	}

	fc.SendMulti(213, "Status follows:", strings.Join(status, "\r\n"), "End of status")
	return nil
}

func (fc *FtpConn) handleMDTM() error {
	path := fc.buildPath(fc.arg)
	fi, err := fc.driver.Stat(path)
	if err != nil {
		fc.Send(550, "Could not get file modification time.")
		return err
	}
	fc.Send(213, fi.ModTime().UTC().Format("20060102150405"))
	return nil
}

func (fc *FtpConn) handleMFMT() error {
	arg := strings.SplitN(fc.arg, " ", 2)
	if len(arg) != 2 {
		fc.Send(500, "Illegal MFMT command.")
		return nil
	}

	mtime, err := time.Parse("20060102150405", arg[0])
	if err != nil {
		fc.Send(500, "Illegal MFMT command.")
		return err
	}

	path := fc.buildPath(arg[1])
	err = fc.driver.Chtimes(path, mtime, mtime)
	if err != nil {
		fc.Send(550, "Could not change file modification time.")
		return err
	}
	fc.Send(213, fmt.Sprintf("Modify=%s; %s", arg[0], arg[1]))
	return nil
}

func (fc *FtpConn) handleRETR() error {
	path := fc.buildPath(fc.arg)

	defer func() {
		fc.offset = 0
		fc.CloseFileTransfer()
	}()

	if ftpHandler.FileBeforeGet != nil {
		if !ftpHandler.FileBeforeGet(fc.user, path) {
			fc.Send(550, "Not Allowed.")
			<-fc.notify
			return nil
		}
	}

	size, reader, err := fc.driver.GetFile(path, fc.offset)
	if err != nil {
		fc.Send(550, "Failed to open file.")
		<-fc.notify
		return err
	}
	defer reader.Close()

	<-fc.notify
	fc.Send(150, fmt.Sprintf("Opening %s mode data connection for %s (%d bytes).", fc.mode, fc.arg, size))
	err = fc.PutFileTransfer(reader)
	if err != nil {
		fc.Send(426, "Failure writing network stream.")
		return err
	}
	fc.Send(226, "Transfer complete.")
	if ftpHandler.FileAfterGet != nil {
		ftpHandler.FileAfterGet(fc.user, path)
	}
	return nil
}

func (fc *FtpConn) handleSTOR() error {
	path := fc.buildPath(fc.arg)

	defer func() {
		fc.offset = 0
		fc.CloseFileTransfer()
	}()

	if ftpHandler.FileBeforePut != nil {
		if !ftpHandler.FileBeforePut(fc.user, path) {
			fc.Send(550, "Not Allowed.")
			<-fc.notify
			return nil
		}
	}

	<-fc.notify
	reader := fc.GetFileTransfer()
	if reader == nil {
		fc.Send(550, "Failed to open transfer.")
		return nil
	}
	fc.Send(150, "Ok to send data.")
	_, err := fc.driver.PutFile(path, fc.offset, reader)
	if err != nil {
		fc.Send(426, "Failure reading network stream.")
		return err
	}
	fc.Send(226, "Transfer complete.")
	if ftpHandler.FileAfterPut != nil {
		ftpHandler.FileAfterPut(fc.user, path)
	}
	return nil
}

func (fc *FtpConn) handleAPPE() error {
	path := fc.buildPath(fc.arg)

	defer func() {
		fc.offset = 0
		fc.CloseFileTransfer()
	}()

	<-fc.notify
	reader := fc.GetFileTransfer()
	if reader == nil {
		fc.Send(550, "Failed to open transfer.")
		return nil
	}
	fc.Send(150, "Ok to send data.")
	_, err := fc.driver.PutFile(path, fc.offset, reader)
	if err != nil {
		fc.Send(426, "Failure reading network stream.")
		return err
	}
	fc.Send(226, "Transfer complete.")
	return nil
}

func (fc *FtpConn) handleDELE() error {
	path := fc.buildPath(fc.arg)

	if ftpHandler.FileBeforeDelete != nil {
		if !ftpHandler.FileBeforeDelete(fc.user, path) {
			fc.Send(550, "Not Allowed.")
			return nil
		}
	}

	err := fc.driver.DeleteFile(path)
	if err != nil {
		fc.Send(550, "Delete operation failed.")
		return err
	}
	fc.Send(250, "Delete operation successful.")
	if ftpHandler.FileAfterDelete != nil {
		ftpHandler.FileAfterDelete(fc.user, path)
	}
	return nil
}

func (fc *FtpConn) handleRNFR() error {
	path := fc.buildPath(fc.arg)

	_, err := fc.driver.Stat(path)
	if err != nil {
		fc.Send(550, "RNFR command failed.")
		return err
	}
	fc.rename = path
	fc.Send(350, "Ready for RNTO.")
	return nil
}

func (fc *FtpConn) handleRNTO() error {
	if fc.rename == "" {
		fc.Send(503, "RNFR required first.")
		return nil
	}
	path := fc.buildPath(fc.arg)

	if ftpHandler.FileBeforeRename != nil {
		if !ftpHandler.FileBeforeRename(fc.user, fc.rename, path) {
			fc.Send(550, "Not Allowed.")
			return nil
		}
	}

	err := fc.driver.Rename(fc.rename, path)
	defer func() {
		fc.rename = ""
	}()
	if err != nil {
		fc.Send(550, "Rename failed.")
		return err
	}
	fc.Send(250, "Rename successful.")
	if ftpHandler.FileAfterRename != nil {
		ftpHandler.FileAfterRename(fc.user, fc.rename, path)
	}
	return nil
}

func (fc *FtpConn) handleALLO() error {
	fc.Send(202, "Obsolete.")
	return nil
}

func (fc *FtpConn) handleREST() error {
	fc.offset, _ = strconv.ParseInt(fc.arg, 10, 0)
	fc.Send(350, fmt.Sprintf("Restart position accepted (%d).", fc.offset))
	return nil
}

func (fc *FtpConn) handleSITE() error {
	fc.Send(202, "@zhoukk")
	return nil
}

func (fc *FtpConn) handleCWD() error {
	path := fc.buildPath(fc.arg)

	fi, err := fc.driver.Stat(path)
	if err != nil || !fi.IsDir() {
		fc.Send(550, "Failed to change directory.")
		return err
	}

	fc.path = path
	fc.Send(250, "Directory successfully changed.")
	return nil
}

func (fc *FtpConn) handlePWD() error {
	fc.Send(257, fmt.Sprintf(`"%s"`, fc.path))
	return nil
}

func (fc *FtpConn) handleCDUP() error {
	path := fc.buildPath("..")

	fi, err := fc.driver.Stat(path)
	if err != nil || !fi.IsDir() {
		fc.Send(550, "Failed to change directory.")
		return err
	}

	fc.path = path
	fc.Send(250, "Directory successfully changed.")
	return nil
}

func (fc *FtpConn) handleNLST() error {
	path := fc.buildPath(fc.arg)

	fc.Send(150, "Here comes the directory listing.")
	defer fc.CloseFileTransfer()

	var files []string
	err := fc.driver.ListDir(path, func(fi FileInfo) error {
		files = append(files, fi.Name())
		return nil
	})
	if err != nil {
		fc.Send(226, "Transfer done (but failed to open directory).")
		<-fc.notify
		return err
	}

	<-fc.notify
	fc.WriteFileTransfer([]byte(strings.Join(files, "\r\n")))
	fc.Send(226, "Directory send OK.")
	return nil
}

func (fc *FtpConn) handleLIST() error {
	path := fc.buildPath(fc.arg)

	fc.Send(150, "Here comes the directory listing.")
	defer fc.CloseFileTransfer()

	var files []string
	err := fc.driver.ListDir(path, func(fi FileInfo) error {
		files = append(files, fc.fileStat(fi))
		return nil
	})
	if err != nil {
		fc.Send(226, "Transfer done (but failed to open directory).")
		<-fc.notify
		return err
	}

	<-fc.notify
	fc.WriteFileTransfer([]byte(strings.Join(files, "\r\n")))
	fc.Send(226, "Directory send OK.")
	return nil
}

func (fc *FtpConn) handleMLSD() error {
	path := fc.buildPath(fc.arg)

	fc.Send(150, "Here comes the directory listing.")
	defer fc.CloseFileTransfer()

	var files []string
	err := fc.driver.ListDir(path, func(fi FileInfo) error {
		files = append(files, fc.fileMls(fi))
		return nil
	})
	if err != nil {
		fc.Send(226, "Transfer done (but failed to open directory).")
		<-fc.notify
		return err
	}

	<-fc.notify
	fc.WriteFileTransfer([]byte(strings.Join(files, "\r\n")))
	fc.Send(226, "Directory send OK.")
	return nil
}

func (fc *FtpConn) handleMLST() error {
	path := fc.buildPath(fc.arg)

	fi, err := fc.driver.Stat(path)
	if err != nil {

		return err
	}
	fc.SendMulti(250, "File details:", fc.fileMls(fi), "End")
	return nil
}

func (fc *FtpConn) handleMKD() error {
	path := fc.buildPath(fc.arg)

	err := fc.driver.MakeDir(path)
	if err != nil {
		fc.Send(550, "Create directory operation failed.")
		return err
	}
	fc.Send(257, fmt.Sprintf(`"%s" created`, fc.quote(path)))
	return nil
}

func (fc *FtpConn) handleRMD() error {
	path := fc.buildPath(fc.arg)

	err := fc.driver.DeleteDir(path)
	if err != nil {
		fc.Send(550, "Remove directory operation failed.")
		return err
	}
	fc.Send(250, "Remove directory operation successful.")
	return nil
}

func (fc *FtpConn) handleTYPE() error {
	switch fc.arg {
	case "A", "a":
		fc.mode = "ASCII"
		fc.Send(200, "Switching to ASCII mode.")
	case "I", "i":
		fc.mode = "BINARY"
		fc.Send(200, "Switching to Binary mode.")
	default:
		fc.mode = ""
		fc.Send(500, "Unrecognised TYPE command.")
	}
	return nil
}

func (fc *FtpConn) handlePASV() error {
	if !fc.config.Pasv.Enable {
		fc.Send(421, "PASV command is disabled.")
		return nil
	}

	if ftpHandler.ClientBeforePasv != nil {
		if !ftpHandler.ClientBeforePasv(fc.user) {
			fc.Send(550, "Not Allowed.")
			return nil
		}
	}

	listener, err := fc.pasvListen()
	if err != nil {
		log.Printf("[%d] pasv listen fail, err: %v\n", fc.id, err)
		return err
	}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("[%d] pasv accept fail, err: %v\n", fc.id, err)
		} else {
			fc.OpenFileTransfer(conn)
		}
		fc.notify <- 1
		listener.Close()
	}()

	ip := fc.config.Pasv.IP
	if len(ip) == 0 {
		ip = fc.ctrlConn.LocalAddr().(*net.TCPAddr).IP.String()
	}
	port := listener.Addr().(*net.TCPAddr).Port
	quads := strings.Split(ip, ".")
	p1 := port / 256
	p2 := port - (p1 * 256)
	fc.Send(227, fmt.Sprintf("Entering Passive Mode (%s,%s,%s,%s,%d,%d).", quads[0], quads[1], quads[2], quads[3], p1, p2))
	return nil
}

func (fc *FtpConn) handlePORT() error {
	if !fc.config.Port.Enable {
		fc.Send(421, "PORT command is disabled.")
		return nil
	}

	if ftpHandler.ClientBeforePort != nil {
		if !ftpHandler.ClientBeforePort(fc.user) {
			fc.Send(550, "Not Allowed.")
			return nil
		}
	}

	quads := strings.Split(fc.arg, ",")
	if len(quads) < 6 {
		fc.Send(500, "Illegal PORT command.")
		return nil
	}
	p1, _ := strconv.Atoi(quads[4])
	p2, _ := strconv.Atoi(quads[5])
	port := (p1 * 256) + p2
	ip := quads[0] + "." + quads[1] + "." + quads[2] + "." + quads[3]

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), time.Duration(fc.config.Port.ConnectTimeout)*time.Second)
	if err != nil {
		fc.Send(500, "Illegal PORT command.")
		return err
	}
	fc.OpenFileTransfer(conn)
	fc.notify <- 1
	fc.Send(200, "PORT command successful.")
	return nil
}

// NewFtpConn return a new ftp session
func NewFtpConn(cid int, conn net.Conn, config *FtpdConfig, tlsConfig *tls.Config, factory DriverFactory) *FtpConn {
	fc := new(FtpConn)

	fc.id = cid
	fc.ctrlConn = conn
	fc.config = config
	fc.tlsConfig = tlsConfig
	fc.reader = bufio.NewReader(conn)
	fc.writer = bufio.NewWriter(conn)
	fc.factory = factory
	fc.path = "/"
	fc.arg = ""
	fc.mode = "ASCII"
	fc.authd = false
	fc.notify = make(chan int, 1)

	return fc
}

// buildPath return ftp clean path
func (fc *FtpConn) buildPath(path string) string {
	if strings.HasPrefix(path, "/") {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(fc.path, path))
}

// fileStat return ftp format file information
func (fc *FtpConn) fileStat(fi FileInfo) string {
	return fmt.Sprintf("%s 1 %s %s %12d %s %s", fi.Mode().String(), fc.user, fc.user, fi.Size(), fi.ModTime().Format("Jan _2 15:04"), fi.Name())
}

// fileMls return ftp mls* command required format file information
func (fc *FtpConn) fileMls(fi FileInfo) string {
	var t string
	if fi.IsDir() {
		t = "dir"
	} else {
		t = "file"
	}
	return fmt.Sprintf("Type=%s;Size=%d;Modify=%s; %s", t, fi.Size(), fi.ModTime().Format("20060102150405"), fi.Name())
}

// quote return quoted string
func (fc *FtpConn) quote(s string) string {
	if !strings.Contains(s, "\"") {
		return s
	}
	return strings.ReplaceAll(s, "\"", `""`)
}

func (fc *FtpConn) pasvListen() (*net.TCPListener, error) {
	nAttempts := fc.config.Pasv.PortEnd - fc.config.Pasv.PortStart + 1

	for i := 0; i < nAttempts; i++ {
		port := fc.config.Pasv.PortStart + rand.Intn(nAttempts)
		laddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return nil, err
		}
		listener, err := net.ListenTCP("tcp", laddr)
		if err == nil {
			fc.pasvPort = port
			listener.SetDeadline(time.Now().Add(time.Duration(fc.config.Pasv.ListenTimeout) * time.Second))
			return listener, err
		}
	}
	return nil, errors.New("no available listening port")
}

// Close close ftp connections
func (fc *FtpConn) Close() {
	if fc.ctrlConn != nil {
		fc.ctrlConn.Close()
		fc.ctrlConn = nil
	}
}

// OpenFileTransfer open a ftp file transfer
func (fc *FtpConn) OpenFileTransfer(conn net.Conn) {
	fc.lock.Lock()
	defer fc.lock.Unlock()
	if fc.dataConn != nil {
		fc.dataConn.Close()
	}
	if fc.config.Debug {
		log.Printf("[%d] Open: %d\n", fc.id, fc.pasvPort)
	}
	fc.dataConn = conn
}

// CloseFileTransfer close a ftp file transfer
func (fc *FtpConn) CloseFileTransfer() {
	fc.lock.Lock()
	defer fc.lock.Unlock()
	if fc.dataConn != nil {
		fc.dataConn.Close()
		fc.dataConn = nil
		if fc.config.Debug {
			log.Printf("[%d] Close: %d\n", fc.id, fc.pasvPort)
		}
		fc.pasvPort = 0
	}
}

// GetFileTransfer return a client file reader transfer
func (fc *FtpConn) GetFileTransfer() io.Reader {
	fc.lock.Lock()
	defer fc.lock.Unlock()
	return fc.dataConn
}

// PutFileTransfer transfer a ftp file to client
func (fc *FtpConn) PutFileTransfer(reader io.Reader) error {
	fc.lock.Lock()
	defer fc.lock.Unlock()
	_, err := io.Copy(fc.dataConn, reader)
	return err
}

// WriteFileTransfer write data to file transfer
func (fc *FtpConn) WriteFileTransfer(msg []byte) {
	fc.lock.Lock()
	defer fc.lock.Unlock()
	if fc.dataConn != nil {
		if fc.config.Debug {
			log.Printf("[%d] Send: %s\n", fc.id, string(msg))
		}
		fc.dataConn.Write(msg)
	}
}

// Send send code and message to client
func (fc *FtpConn) Send(code int, msg string) {
	if fc.config.Debug {
		log.Printf("[%d] Send: %d %s\n", fc.id, code, msg)
	}
	fc.writer.WriteString(fmt.Sprintf("%d %s\r\n", code, msg))
	fc.writer.Flush()
}

// SendMulti send code and multiple line message to client
func (fc *FtpConn) SendMulti(code int, header, body, footer string) {
	if fc.config.Debug {
		log.Printf("[%d] Send %d %s\n%s\n%s\n", fc.id, code, header, body, footer)
	}
	fc.writer.WriteString(fmt.Sprintf("%d-%s\r\n%s\r\n%d %s\r\n", code, header, body, code, footer))
	fc.writer.Flush()
}

// Serve parse and handle ftp client data
func (fc *FtpConn) Serve() {
	fc.Send(220, "KFtpd")
	for {
		line, _, err := fc.reader.ReadLine()
		if err != nil {
			break
		}
		if len(line) == 0 {
			continue
		}
		if fc.config.Debug {
			log.Printf("[%d] Recv: %v\n", fc.id, string(line))
		}
		words := strings.SplitN(string(line), " ", 2)
		command := strings.ToUpper(words[0])
		if len(words) == 2 {
			fc.arg = words[1]
		} else {
			fc.arg = ""
		}
		if command == "HELP" {
			var cmds []string
			for cmd := range cmdMap {
				cmds = append(cmds, " "+cmd)
			}
			sort.Strings(cmds)
			fc.SendMulti(214, "The following commands are recognized.", strings.Join(cmds, "\r\n"), "Help OK.")
			continue
		}
		cmd, ok := cmdMap[command]
		if !ok {
			fc.Send(500, "Unknown command.")
			continue
		}
		if cmd.Auth && !fc.authd {
			fc.Send(530, "Please login with USER and PASS.")
			continue
		}
		if err := cmd.Fn(fc); err != nil {
			log.Printf("[%d] %s: %v\n", fc.id, command, err)
		}
	}
	fc.Close()
}

// UserBeforeLogin register
func UserBeforeLogin(handler func(string, string) bool) {
	ftpHandler.UserBeforeLogin = handler
}

// UserAfterLogin register
func UserAfterLogin(handler func(string)) {
	ftpHandler.UserAfterLogin = handler
}

// ClientBeforePasv register
func ClientBeforePasv(handler func(string) bool) {
	ftpHandler.ClientBeforePasv = handler
}

// ClientBeforePort register
func ClientBeforePort(handler func(string) bool) {
	ftpHandler.ClientBeforePort = handler
}

// FileBeforePut register
func FileBeforePut(handler func(string, string) bool) {
	ftpHandler.FileBeforePut = handler
}

// FileAfterPut register
func FileAfterPut(handler func(string, string)) {
	ftpHandler.FileAfterPut = handler
}

// FileBeforeGet register
func FileBeforeGet(handler func(string, string) bool) {
	ftpHandler.FileBeforeGet = handler
}

// FileAfterGet register
func FileAfterGet(handler func(string, string)) {
	ftpHandler.FileAfterGet = handler
}

// FileBeforeDelete register
func FileBeforeDelete(handler func(string, string) bool) {
	ftpHandler.FileBeforeDelete = handler
}

// FileAfterDelete register
func FileAfterDelete(handler func(string, string)) {
	ftpHandler.FileAfterDelete = handler
}

// FileBeforeRename register
func FileBeforeRename(handler func(string, string, string) bool) {
	ftpHandler.FileBeforeRename = handler
}

// FileAfterRename register
func FileAfterRename(handler func(string, string, string)) {
	ftpHandler.FileAfterRename = handler
}

var factory DriverFactory

// SetDriverFactory set a custom ftp driver factory
func SetDriverFactory(customDriverFactory DriverFactory) {
	factory = customDriverFactory
}

// NewFtpdConfig return a ftd config
func NewFtpdConfig() *FtpdConfig {
	var cfg FtpdConfig

	cfg.Bind = ":21"
	cfg.Driver = "file"
	cfg.HomeDir = true
	cfg.Debug = true

	cfg.Pasv.Enable = true
	cfg.Pasv.IP = ""
	cfg.Pasv.PortStart = 21000
	cfg.Pasv.PortEnd = 21100
	cfg.Pasv.ListenTimeout = 10

	cfg.Port.Enable = true
	cfg.Port.ConnectTimeout = 10

	cfg.FileDriver.BaseDir = "kftpd-data"

	cfg.MinioDriver.Endpoint = "127.0.0.1:9000"
	cfg.MinioDriver.AccessKeyID = "minioadmin"
	cfg.MinioDriver.SecretAccessKey = "minioadmin"
	cfg.MinioDriver.Bucket = "kftpd-data"
	cfg.MinioDriver.UseSSL = false

	cfg.AuthTLS.Enable = false
	cfg.AuthTLS.CertFile = ""
	cfg.AuthTLS.KeyFile = ""

	cfg.Users = map[string]string{
		"kftpd": "kftpd",
	}

	if env, ok := os.LookupEnv("KFTPD_BIND"); ok {
		cfg.Bind = env
	}

	if env, ok := os.LookupEnv("KFTPD_DRIVER"); ok {
		cfg.Driver = env
	}

	if env, ok := os.LookupEnv("KFTPD_HOMEDIR"); ok {
		cfg.HomeDir, _ = strconv.ParseBool(env)
	}

	if env, ok := os.LookupEnv("KFTPD_DEBUG"); ok {
		cfg.Debug, _ = strconv.ParseBool(env)
	}

	if env, ok := os.LookupEnv("KFTPD_PASV_ENABLE"); ok {
		cfg.Pasv.Enable, _ = strconv.ParseBool(env)
	}

	if env, ok := os.LookupEnv("KFTPD_PASV_IP"); ok {
		cfg.Pasv.IP = env
	}

	if env, ok := os.LookupEnv("KFTPD_PASV_PORTSTART"); ok {
		cfg.Pasv.PortStart, _ = strconv.Atoi(env)
	}

	if env, ok := os.LookupEnv("KFTPD_PASV_PORTEND"); ok {
		cfg.Pasv.PortEnd, _ = strconv.Atoi(env)
	}

	if env, ok := os.LookupEnv("KFTPD_PASV_LISTEN_TIMEOUT"); ok {
		cfg.Pasv.ListenTimeout, _ = strconv.Atoi(env)
	}

	if env, ok := os.LookupEnv("KFTPD_PORT_ENABLE"); ok {
		cfg.Port.Enable, _ = strconv.ParseBool(env)
	}

	if env, ok := os.LookupEnv("KFTPD_PORT_CONNECT_TIMEOUT"); ok {
		cfg.Port.ConnectTimeout, _ = strconv.Atoi(env)
	}

	if env, ok := os.LookupEnv("KFTPD_FILEDRIVER_BASEDIR"); ok {
		cfg.FileDriver.BaseDir = env
	}

	if env, ok := os.LookupEnv("KFTPD_MINIODRIVER_ENDPOINT"); ok {
		cfg.MinioDriver.Endpoint = env
	}

	if env, ok := os.LookupEnv("KFTPD_MINIODRIVER_ACCESSKEYID"); ok {
		cfg.MinioDriver.AccessKeyID = env
	}

	if env, ok := os.LookupEnv("KFTPD_MINIODRIVER_SECRETACCESSKEY"); ok {
		cfg.MinioDriver.SecretAccessKey = env
	}

	if env, ok := os.LookupEnv("KFTPD_MINIODRIVER_BUCKET"); ok {
		cfg.MinioDriver.Bucket = env
	}

	if env, ok := os.LookupEnv("KFTPD_MINIODRIVER_USESSL"); ok {
		cfg.MinioDriver.UseSSL, _ = strconv.ParseBool(env)
	}

	if env, ok := os.LookupEnv("KFTPD_AUTHTLS_ENABLE"); ok {
		cfg.AuthTLS.Enable, _ = strconv.ParseBool(env)
	}

	if env, ok := os.LookupEnv("KFTPD_AUTHTLS_CERTFILE"); ok {
		cfg.AuthTLS.CertFile = env
	}

	if env, ok := os.LookupEnv("KFTPD_AUTHTLS_KEYFILE"); ok {
		cfg.AuthTLS.KeyFile = env
	}

	if env, ok := os.LookupEnv("KFTPD_USERS"); ok {
		cfg.Users = make(map[string]string)
		arr := strings.Split(env, ",")
		for _, v := range arr {
			s := strings.Split(v, ":")
			if len(s) == 2 {
				cfg.Users[s[0]] = s[1]
			}
		}
	}

	return &cfg
}

// LoadFtpdConfig return a ftd config loaded from config file
func LoadFtpdConfig(configFile string) (*FtpdConfig, error) {
	cfg := NewFtpdConfig()

	if len(configFile) > 0 {
		data, err := ioutil.ReadFile(configFile)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// FtpdServe start the ftp server
func FtpdServe(config *FtpdConfig) error {
	var tlsConfig *tls.Config
	if config.AuthTLS.Enable {
		cert, err := tls.LoadX509KeyPair(config.AuthTLS.CertFile, config.AuthTLS.KeyFile)
		if err != nil {
			return err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	} else {
		tlsConfig = nil
	}

	switch config.Driver {
	case "file":
		factory = NewFileDriverFactory(config.FileDriver.BaseDir)
	case "minio":
		factory = NewMinioDriverFactory(config.MinioDriver.Endpoint, config.MinioDriver.AccessKeyID, config.MinioDriver.SecretAccessKey, config.MinioDriver.Bucket, config.MinioDriver.UseSSL)
	case "custom":
	default:
		return fmt.Errorf("not supported driver: %s", config.Driver)
	}

	listener, err := net.Listen("tcp", config.Bind)
	if err != nil {
		return err
	}

	cid := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go NewFtpConn(cid, conn, config, tlsConfig, factory).Serve()
		cid = cid + 1
	}
}
