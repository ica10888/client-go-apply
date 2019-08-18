package kubectl

import (
	"archive/tar"
	"fmt"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	_ "k8s.io/kubernetes/pkg/kubectl/cmd/cp"
	k8sexec "k8s.io/kubernetes/pkg/kubectl/cmd/exec"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	_ "unsafe"
)

func (i *Pod) Cp(srcPath string, destPath string) error {
	restconfig, err, coreclient := InitRestClient()

	reader, writer := io.Pipe()
	// strip trailing slash (if any)
	if destPath != "/" && strings.HasSuffix(string(destPath[len(destPath)-1]), "/") {
		destPath = destPath[:len(destPath)-1]
	}
	if err := checkDestinationIsDir(i, destPath); err == nil {
		// If no error, destPath was found to be a directory.
		// Copy specified src into it
		destPath = destPath + "/" + path.Base(srcPath)
	}
	go func() {
		defer writer.Close()
		err := cpMakeTar(srcPath, destPath, writer)
		cmdutil.CheckErr(err)
	}()
	var cmdArr []string

	cmdArr = []string{"tar", "-xf", "-"}
	destDir := path.Dir(destPath)
	if len(destDir) > 0 {
		cmdArr = append(cmdArr, "-C", destDir)
	}
	// Prepare the API URL used to execute another process within the Pod.  In
	// this case, we'll run a remote shell.
	req := coreclient.RESTClient().
		Post().
		Namespace(i.Namespace).
		Resource("pods").
		Name(i.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: i.ContainerName,
			Command:   cmdArr,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restconfig, "POST", req.URL())
	if err != nil {
		log.Fatalf("error %s\n", err)
		return err
	}
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  reader,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Tty:    false,
	})
	if err != nil {
		log.Fatalf("error %s\n", err)
		return err
	}
	return nil
}

func checkDestinationIsDir(i *Pod, destPath string) error {
	return i.Exec([]string{"test", "-d", destPath})
}

//go:linkname cpMakeTar k8s.io/kubernetes/pkg/kubectl/cmd/cp.makeTar
func cpMakeTar(srcPath, destPath string, writer io.Writer) error

func (i *Pod) Cp2(srcPath string, destPath string) error {

	reader, outStream := io.Pipe()
	options := &k8sexec.ExecOptions{
		StreamOptions: k8sexec.StreamOptions{
			IOStreams: genericclioptions.IOStreams{
				In:     nil,
				Out:    outStream,
				ErrOut: os.Stderr,
			},

			Namespace: i.Namespace,
			PodName:   i.Name,
		},

		Command:  []string{"tar", "cf", "-", srcPath},
		Executor: &k8sexec.DefaultRemoteExecutor{},
	}

	go func() {
		defer outStream.Close()
		options.Config, _, _ = InitRestClient()
		options.PodClient = CoreV1Client()
		_ = options.Run()
		//cmdutil.CheckErr(err)
	}()
	prefix := getPrefix(srcPath)
	prefix = path.Clean(prefix)
	// remove extraneous path shortcuts - these could occur if a path contained extra "../"
	// and attempted to navigate beyond "/" in a remote filesystem
	prefix = cpStripPathShortcuts(prefix)
	return untarAll(reader, destPath, prefix)
}

func getPrefix(file string) string {
	// tar strips the leading '/' if it's there, so we will too
	return strings.TrimLeft(file, "/")
}

//go:linkname cpStripPathShortcuts k8s.io/kubernetes/pkg/kubectl/cmd/cp.stripPathShortcuts
func cpStripPathShortcuts(p string) string

func untarAll(reader io.Reader, destDir, prefix string) error {
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}

		// All the files will start with the prefix, which is the directory where
		// they were located on the pod, we need to strip down that prefix, but
		// if the prefix is missing it means the tar was tempered with.
		// For the case where prefix is empty we need to ensure that the path
		// is not absolute, which also indicates the tar file was tempered with.
		if !strings.HasPrefix(header.Name, prefix) {
			return fmt.Errorf("tar contents corrupted")
		}

		// basic file information
		mode := header.FileInfo().Mode()
		destFileName := filepath.Join(destDir, header.Name[len(prefix):])

		baseName := filepath.Dir(destFileName)
		if err := os.MkdirAll(baseName, 0755); err != nil {
			return err
		}
		if header.FileInfo().IsDir() {
			if err := os.MkdirAll(destFileName, 0755); err != nil {
				return err
			}
			continue
		}

		// We need to ensure that the destination file is always within boundries
		// of the destination directory. This prevents any kind of path traversal
		// from within tar archive.
		evaledPath, err := filepath.EvalSymlinks(baseName)
		if err != nil {
			return err
		}

		if mode&os.ModeSymlink != 0 {
			linkname := header.Linkname
			// We need to ensure that the link destination is always within boundries
			// of the destination directory. This prevents any kind of path traversal
			// from within tar archive.
			if !filepath.IsAbs(linkname) {
				_ = filepath.Join(evaledPath, linkname)
			}

			if err := os.Symlink(linkname, destFileName); err != nil {
				return err
			}
		} else {
			outFile, err := os.Create(destFileName)
			if err != nil {
				return err
			}
			defer outFile.Close()
			if _, err := io.Copy(outFile, tarReader); err != nil {
				return err
			}
			if err := outFile.Close(); err != nil {
				return err
			}
		}
	}

	return nil
}
