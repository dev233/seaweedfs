//go:build linux || darwin
// +build linux darwin

package command

import (
	"context"
	"fmt"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/mount/unmount"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc/reflection"
	"net"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/grace"
)

func runMount(cmd *Command, args []string) bool {

	if *mountOptions.debug {
		go http.ListenAndServe(fmt.Sprintf(":%d", *mountOptions.debugPort), nil)
	}

	grace.SetupProfiling(*mountCpuProfile, *mountMemProfile)
	if *mountReadRetryTime < time.Second {
		*mountReadRetryTime = time.Second
	}
	util.RetryWaitTime = *mountReadRetryTime

	umask, umaskErr := strconv.ParseUint(*mountOptions.umaskString, 8, 64)
	if umaskErr != nil {
		fmt.Printf("can not parse umask %s", *mountOptions.umaskString)
		return false
	}

	if len(args) > 0 {
		return false
	}

	return RunMount(&mountOptions, os.FileMode(umask))
}

func RunMount(option *MountOptions, umask os.FileMode) bool {
	startTime := time.Now()

	filerMountRootPath := *option.filerMountRootPath
	if util.FullPath(filerMountRootPath).IsQuotaRootNode() {
		if option.DirectoryQuotaSize != nil {
			_, err := util.ParseBytes(*option.DirectoryQuotaSize)
			if err != nil {
				glog.Errorf("failed to parse DirectoryQuotaSize %v: %v", option.DirectoryQuotaSize, err)
				return true
			}
		}
	}

	// basic checks
	chunkSizeLimitMB := *mountOptions.chunkSizeLimitMB
	if chunkSizeLimitMB <= 0 {
		fmt.Printf("Please specify a reasonable buffer size.")
		return false
	}

	// try to connect to filer
	filerAddresses := pb.ServerAddresses(*option.filer).ToAddresses()
	util.LoadConfiguration("security", false)

	//TODO: 这里使用 WithUserAgent, 向filer发送鉴权信息，filer从对应用户目录的xattr中记录的token来鉴定权限
	// grpc.WithUserAgent("userID=user-access-token")
	// pkg/mod/google.golang.org/grpc@v1.47.0/dialoptions.go:407
	grpcDialOption := security.LoadClientTLS(util.GetViper(), "grpc.client")
	var cipher bool
	var err error
	for i := 0; i < 10; i++ {
		err = pb.WithOneOfGrpcFilerClients(false, filerAddresses, grpcDialOption, func(client filer_pb.SeaweedFilerClient) error {
			resp, err := client.GetFilerConfiguration(context.Background(), &filer_pb.GetFilerConfigurationRequest{})
			if err != nil {
				return fmt.Errorf("get filer grpc address %v configuration: %v", filerAddresses, err)
			}
			cipher = resp.Cipher
			return nil
		})
		if err != nil {
			glog.V(0).Infof("failed to talk to filer %v: %v", filerAddresses, err)
			glog.V(0).Infof("wait for %d seconds ...", i+1)
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
	if err != nil {
		glog.Errorf("failed to talk to filer %v: %v", filerAddresses, err)
		return true
	}

	// clean up mount point
	dir := util.ResolvePath(*option.dir)
	if dir == "" {
		fmt.Printf("Please specify the mount directory via \"-dir\"")
		return false
	}

	unmount.Unmount(dir)

	// start on local unix socket
	if *option.localSocket == "" {
		mountDirHash := util.HashToInt32([]byte(dir))
		if mountDirHash < 0 {
			mountDirHash = -mountDirHash
		}
		*option.localSocket = fmt.Sprintf("/tmp/seaweefs-mount-%d.sock", mountDirHash)
	}
	if err := os.Remove(*option.localSocket); err != nil && !os.IsNotExist(err) {
		glog.Fatalf("Failed to remove %s, error: %s", *option.localSocket, err.Error())
	}
	montSocketListener, err := net.Listen("unix", *option.localSocket)
	if err != nil {
		glog.Fatalf("Failed to listen on %s: %v", *option.localSocket, err)
	}

	// detect mount folder mode
	if *option.dirAutoCreate {
		os.MkdirAll(dir, os.FileMode(0777)&^umask)
	}
	fileInfo, err := os.Stat(dir)

	// collect uid, gid
	uid, gid := uint32(0), uint32(0)
	mountMode := os.ModeDir | 0777
	if err == nil {
		mountMode = os.ModeDir | os.FileMode(0777)&^umask
		uid, gid = util.GetFileUidGid(fileInfo)
		fmt.Printf("mount point owner uid=%d gid=%d mode=%s\n", uid, gid, mountMode)
	} else {
		fmt.Printf("can not stat %s\n", dir)
		return false
	}

	// detect uid, gid
	if uid == 0 {
		if u, err := user.Current(); err == nil {
			if parsedId, pe := strconv.ParseUint(u.Uid, 10, 32); pe == nil {
				uid = uint32(parsedId)
			}
			if parsedId, pe := strconv.ParseUint(u.Gid, 10, 32); pe == nil {
				gid = uint32(parsedId)
			}
			fmt.Printf("current uid=%d gid=%d\n", uid, gid)
		}
	}

	// mapping uid, gid
	uidGidMapper, err := meta_cache.NewUidGidMapper(*option.uidMap, *option.gidMap)
	if err != nil {
		fmt.Printf("failed to parse %s %s: %v\n", *option.uidMap, *option.gidMap, err)
		return false
	}

	// Ensure target mount point availability
	if isValid := checkMountPointAvailable(dir); !isValid {
		glog.Fatalf("Expected mount to still be active, target mount point: %s, please check!", dir)
		return true
	}

	serverFriendlyName := strings.ReplaceAll(*option.filer, ",", "+")

	// mount fuse
	fuseMountOptions := &fuse.MountOptions{
		AllowOther:               *option.allowOthers,
		Options:                  nil,
		MaxBackground:            128,
		MaxWrite:                 1024 * 1024 * 2,
		MaxReadAhead:             1024 * 1024 * 2,
		IgnoreSecurityLabels:     false,
		RememberInodes:           false,
		FsName:                   "adfs:" + filerMountRootPath,
		Name:                     "adfs",
		SingleThreaded:           false,
		DisableXAttrs:            *option.disableXAttr,
		Debug:                    *option.debug,
		EnableLocks:              false,
		ExplicitDataCacheControl: false,
		DirectMount:              true,
		DirectMountFlags:         0,
		//SyncRead:                 false, // set to false to enable the FUSE_CAP_ASYNC_READ capability
		//EnableAcl:                true,
	}
	if *option.nonempty {
		fuseMountOptions.Options = append(fuseMountOptions.Options, "nonempty")
	}
	if *option.readOnly {
		if runtime.GOOS == "darwin" {
			fuseMountOptions.Options = append(fuseMountOptions.Options, "rdonly")
		} else {
			fuseMountOptions.Options = append(fuseMountOptions.Options, "ro")
		}
	}
	if runtime.GOOS == "darwin" {
		// https://github-wiki-see.page/m/macfuse/macfuse/wiki/Mount-Options
		ioSizeMB := 1
		for ioSizeMB*2 <= *option.chunkSizeLimitMB && ioSizeMB*2 <= 32 {
			ioSizeMB *= 2
		}
		fuseMountOptions.Options = append(fuseMountOptions.Options, "daemon_timeout=600")
		fuseMountOptions.Options = append(fuseMountOptions.Options, "noapplexattr")
		// fuseMountOptions.Options = append(fuseMountOptions.Options, "novncache") // need to test effectiveness
		fuseMountOptions.Options = append(fuseMountOptions.Options, "slow_statfs")
		fuseMountOptions.Options = append(fuseMountOptions.Options, "volname="+serverFriendlyName)
		fuseMountOptions.Options = append(fuseMountOptions.Options, fmt.Sprintf("iosize=%d", ioSizeMB*1024*1024))
	}

	// find mount point
	mountRoot := filerMountRootPath
	if mountRoot != "/" && strings.HasSuffix(mountRoot, "/") {
		mountRoot = mountRoot[0 : len(mountRoot)-1]
	}

	seaweedFileSystem := mount.NewSeaweedFileSystem(&mount.Option{
		MountDirectory:      dir,
		FilerAddresses:      filerAddresses,
		GrpcDialOption:      grpcDialOption,
		FilerMountRootPath:  mountRoot,
		Collection:          *option.collection,
		Replication:         *option.replication,
		TtlSec:              int32(*option.ttlSec),
		DiskType:            types.ToDiskType(*option.diskType),
		ChunkSizeLimit:      int64(chunkSizeLimitMB) * 1024 * 1024,
		ConcurrentWriters:   *option.concurrentWriters,
		ConcurrentReaders:   *option.concurrentReaders,
		CacheDir:            *option.cacheDir,
		CacheSizeMB:         *option.cacheSizeMB,
		DataCenter:          *option.dataCenter,
		Quota:               int64(*option.collectionQuota) * 1024 * 1024,
		MountUid:            uid,
		MountGid:            gid,
		MountMode:           mountMode,
		MountCtime:          fileInfo.ModTime(),
		MountMtime:          time.Now(),
		Umask:               umask,
		VolumeServerAccess:  *mountOptions.volumeServerAccess,
		Cipher:              cipher,
		UidGidMapper:        uidGidMapper,
		DisableXAttr:        *option.disableXAttr,
		ConcurrentLimit:     *option.ConcurrentLimit,
		AuthKey:             *option.AuthKey,
		DirectoryQuotaSize:  *option.DirectoryQuotaSize,
		DirectoryQuotaInode: *option.DirectoryQuotaInode,
	})

	server, err := fuse.NewServer(seaweedFileSystem, dir, fuseMountOptions)
	if err != nil {
		glog.Fatalf("Mount fail: %v", err)
	}
	grace.OnInterrupt(func() {
		unmount.Unmount(dir)
	})

	grpcS := pb.NewGrpcServer()
	mount_pb.RegisterSeaweedMountServer(grpcS, seaweedFileSystem)
	reflection.Register(grpcS)
	go grpcS.Serve(montSocketListener)

	seaweedFileSystem.StartBackgroundTasks()

	fmt.Printf("This is SeaweedFS version %s %s %s, start cost: %s\n", util.Version(), runtime.GOOS, runtime.GOARCH, time.Now().Sub(startTime).String())

	server.Serve()

	return true
}
