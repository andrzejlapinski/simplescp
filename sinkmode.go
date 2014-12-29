package main

import (
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func sendSCPBinaryOK(channel ssh.Channel) error {
	_, err := channel.Write([]byte("\000"))
	return err
}

type controlMessage struct {
	msgType string
	name    string
	mode    os.FileMode
	size    uint64
	mtime   int64
	atime   int64
}

// TODO: Cleanup, send errors on protocol errors
func receiveControlMsg(channel ssh.Channel) (controlMessage, error) {
	ctrlmsg := controlMessage{}

	ctrlmsgbuf := make([]byte, 256)
	nread, err := channel.Read(ctrlmsgbuf)
	if err != nil {
		// TODO: Maybe only return it upstream if it's an EOF, otherwise handle it?
		return ctrlmsg, err
	}
	ctrlmsg.msgType = string(ctrlmsgbuf[0])

	ctrlmsglist := strings.Split(string(ctrlmsgbuf[:nread]), " ")
	log.Println(ctrlmsglist)

	// Make sure control message is valid
	switch string(ctrlmsgbuf[0]) {
	case "E":
		if nread > 2 {
			// TODO: Protocol error
			log.Println("Protocol error, got: ", string(ctrlmsgbuf[:nread]))
			return ctrlmsg, errors.New("Protocol error")
		} else {
			err := sendSCPBinaryOK(channel)
			return ctrlmsg, err
		}
	case "C":
	case "D":
	case "T":
	default:
		// TODO: We have an expected message here, report it and abort
	}

	if ctrlmsg.msgType == "T" {
		ctrlmsg.mtime, err = strconv.ParseInt(ctrlmsglist[0][1:], 10, 64)
		if err != nil {
			return ctrlmsg, errors.New("mtime.sec not delimited")
		}
		ctrlmsg.atime, err = strconv.ParseInt(ctrlmsglist[2], 10, 64)
		if err != nil {
			return ctrlmsg, errors.New("atime.sec not delimited")
		}
		sendSCPBinaryOK(channel)
		// A "T" message will always come before a "D" or "C", so we can combine both
		newCtrlmsg, err := receiveControlMsg(channel)
		if err != nil {
			return ctrlmsg, errors.New("Protocol error")
		}

		newCtrlmsg.mtime = ctrlmsg.mtime
		newCtrlmsg.atime = ctrlmsg.atime

		return newCtrlmsg, nil
	}

	if len(ctrlmsglist) != 3 {
		return ctrlmsg, errors.New("Protocol error")
	}

	ctrlmsg.name = ctrlmsglist[2][:len(ctrlmsglist[2])-1] // Remove trailing newline
	size, err := strconv.ParseInt(ctrlmsglist[1], 10, 64)
	ctrlmsg.size = uint64(size)
	if err != nil {
		return ctrlmsg, errors.New("Protocol error")
	}
	mode, err := strconv.ParseInt(ctrlmsglist[0][1:], 8, 32)
	ctrlmsg.mode = os.FileMode(mode)
	if err != nil {
		return ctrlmsg, errors.New("Protocol error")
	}
	sendSCPBinaryOK(channel)
	return ctrlmsg, nil
}

// Generate a full path out of our basedir, the directories currently in the stack, and the target
func generatePath(dirStack []string, target string) string {
	fullPathList := make([]string, 0)
	fullPathList = append(fullPathList, globalConfig.basedir)
	fullPathList = append(fullPathList, dirStack...)
	fullPathList = append(fullPathList, target)

	path := filepath.Clean(filepath.Join(fullPathList...))

	return path
}

// Receive the contents of a file and store it in the right place
func receiveFileContents(channel ssh.Channel, dirStack []string, msgctrl controlMessage, name string, preserveMode bool) error {

	filename := generatePath(dirStack, name)

	log.Printf("Filename is '%s'", filename)
	// TODO: Make sure we're reporting the right error here if something happens
	f, err := os.Create(filename)
	log.Println(f)
	if err != nil {
		log.Println(err)
		return err
	}
	defer f.Close()
	nread, err := io.CopyN(f, channel, int64(msgctrl.size))
	log.Printf("Transferred %d bytes", nread)
	if err != nil {
		log.Println(err)
		return err
	}

	// TODO: Double check that we're doing the right thing in all cases (file already exists, file doesn't exist, etc)
	err = f.Chmod(msgctrl.mode)
	if err != nil {
		log.Println(err)
		return err
	}

	if preserveMode {
		atime := time.Unix(msgctrl.atime, 0)
		mtime := time.Unix(msgctrl.mtime, 0)
		err := os.Chtimes(filename, atime, mtime)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	statusbuf := make([]byte, 1)
	_, err = channel.Read(statusbuf)
	if err != nil {
		log.Println("Getting status after transfer", err)
		return err
	}
	sendSCPBinaryOK(channel)
	return err
}

// Create a directory, ignore errors if it already exists
func createDir(target string) error {
	// TODO: What permissions should we use here?
	var perm os.FileMode = 0755
	err := os.Mkdir(target, perm)
	if err != nil {
		if e, ok := err.(*os.PathError); ok && e.Err == unix.EEXIST {
			log.Println("File already exists, big deal")
		} else {
			log.Println(err)
			return err
		}
	}
	return nil
}

func startSCPSink(channel ssh.Channel, opts scpOptions) error {
	// If target exists and it's a dir, put all the files in there
	// If target doesn't exist or it's a regular file:
	//   - If we only want to copy one file, use it as destination
	//   - If we want to copy more than one file, it's an error: "No such file or directory" or "Not a directory"

	// Only one target should have been specified
	target := opts.fileNames[0]

	// Target seems to be a directory
	if string(target[len(target)-1]) == "/" {
		opts.TargetIsDir = true
	}

	absTarget := filepath.Clean(filepath.Join(globalConfig.basedir, target))
	if !strings.HasPrefix(absTarget, globalConfig.basedir) {
		// We're attempting to copy files outside of our working directory, so return an error
		msg := fmt.Sprintf("scp: %s: Not a directory", target)
		sendErrorToClient(msg, channel)
		return errors.New(msg)
	}

	dirStack := make([]string, 0)

	if opts.TargetIsDir {
		err := createDir(absTarget)
		if err != nil {
			return err
		}
		dirStack = append(dirStack, target)
	}

	log.Println("Dir stack is", dirStack)

	// Tell the other side we're ready to start receiving data
	sendSCPBinaryOK(channel)
	for {
		ctrlmsg, err := receiveControlMsg(channel)

		if err != nil {
			if err == io.EOF {
				// EOF is fine at this point, it just means no more files to copy
				break
			}
			log.Println("Got error from client:", err)
			break
		}

		log.Println(ctrlmsg.msgType)
		switch ctrlmsg.msgType {
		case "D":
			// TODO: Figure out how we need to behave in terms of permissions/times, etc
			err := createDir(generatePath(dirStack, ctrlmsg.name))
			if err != nil {
				return err
			}
			dirStack = append(dirStack, ctrlmsg.name)
			log.Println("dir stack is now:", dirStack)
		case "E":
			stackSize := len(dirStack)
			if (opts.TargetIsDir && stackSize <= 1) || (!opts.TargetIsDir && stackSize <= 0) {
				msg := "scp: Protocol Error"
				sendErrorToClient(msg, channel)
				return errors.New(msg)
			}
			dirStack = dirStack[:len(dirStack)-1]
		case "C":
			var filename string
			if opts.TargetIsDir {
				filename = ctrlmsg.name
			} else {
				filename = target
			}
			receiveFileContents(channel, dirStack, ctrlmsg, filename, opts.PreserveMode)
		}

		// Steps here:
		// 1. Figure out what kind of message this is (file, directory, time, end of directory...)
		// If it's D:
		//   - Add a new directory to the stack
		//   - Create the directory
		// If it's E:
		//   - Remove one directory from the stack
		// If it's C:
		//   - Receive file and store it in the current stack
		// If it's T:
		//   - Receive the next control message

	}
	return nil
}
