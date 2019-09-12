package exec

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"gitlab.eng.vmware.com/vivienv/flare/script"
	"gitlab.eng.vmware.com/vivienv/flare/ssh"
)

func TestMain(m *testing.M) {
	loglevel := "debug"
	flag.StringVar(&loglevel, "loglevel", loglevel, "Sets log level")
	flag.Parse()

	if parsed, err := logrus.ParseLevel(loglevel); err != nil {
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		logrus.SetLevel(parsed)
	}
	logrus.SetOutput(os.Stdout)

	os.Exit(m.Run())
}

type execTest struct {
	name       string
	source     func() string
	exec       func(*script.Script) error
	shouldFail bool
}

func runExecutorTest(t *testing.T, test execTest) {
	defer func() {
		if _, err := os.Stat(script.Defaults.WorkdirValue); err != nil {
			t.Log(err)
			return
		}
		if err := os.RemoveAll(script.Defaults.WorkdirValue); err != nil {
			t.Log(err)
		}
	}()

	script, err := script.Parse(strings.NewReader(test.source()))
	if err != nil {
		if !test.shouldFail {
			t.Fatal(err)
		}
		t.Log(err)
		return
	}
	if err := test.exec(script); err != nil {
		if !test.shouldFail {
			t.Fatal(err)
		}
		t.Log(err)
	}
}
func makeTestDir(t *testing.T, name string) error {
	if err := os.MkdirAll(name, 0744); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func makeTestFakeFile(t *testing.T, name, content string) error {
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	defer file.Close()
	t.Logf("creating local test file %s", name)
	_, err = io.Copy(file, strings.NewReader(content))
	return err
}

func maketTestSSHClient() (*ssh.SSHClient, error) {
	usr, err := user.Current()
	if err != nil {
		return nil, err
	}

	privKey := filepath.Join(usr.HomeDir, ".ssh/id_rsa")
	return ssh.New(usr.Username, privKey), nil
}

func makeRemoteTestFile(t *testing.T, addr, fileName, content string) error {
	sshc, err := maketTestSSHClient()
	if err != nil {
		return err
	}

	if err := sshc.Dial(addr); err != nil {
		return err
	}
	defer sshc.Hangup()

	t.Logf("creating remote test file %s", fileName)
	_, err = sshc.SSHRun("echo", fmt.Sprintf("\"%s\"", content), ">", fileName)
	if err != nil {
		return err
	}
	return nil
}

func removeRemoteTestFile(t *testing.T, addr, fileName string) error {
	sshc, err := maketTestSSHClient()
	if err != nil {
		return err
	}

	if err := sshc.Dial(addr); err != nil {
		return err
	}
	defer sshc.Hangup()
	t.Logf("removing remote test file %s", fileName)
	_, err = sshc.SSHRun("rm", "-rf", fileName)
	if err != nil {
		return err
	}
	return nil
}

func makeRemoteTestDir(t *testing.T, addr, path string) error {
	sshc, err := maketTestSSHClient()
	if err != nil {
		return err
	}

	if err := sshc.Dial(addr); err != nil {
		return err
	}
	defer sshc.Hangup()
	t.Logf("creating remote test  dir %s", path)
	_, err = sshc.SSHRun("mkdir", "-p", path)
	if err != nil {
		return err
	}
	return nil
}

func TestExecutor_New(t *testing.T) {
	tests := []struct {
		name   string
		script *script.Script
	}{
		{name: "simple script", script: &script.Script{}},
		{name: "nil script"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := New(test.script)
			if s.script != test.script {
				t.Error("unexpected script value")
			}
		})
	}
}

func TestExecutor(t *testing.T) {
	tests := []execTest{
		{
			name: "Executing all commands",
			source: func() string {
				var src strings.Builder
				src.WriteString("# This is a sample comment\n")
				src.WriteString("#### START\n")
				src.WriteString("FROM local\n")
				src.WriteString("WORKDIR /tmp/{{.Username}}\n")
				src.WriteString("CAPTURE /bin/echo HELLO\n")
				src.WriteString("COPY /tmp/buzz.txt\n")
				src.WriteString("ENV MSG0=HELLO MSG1=WORLD\n")
				src.WriteString("CAPTURE ./bar.sh\n")
				src.WriteString("COPY /tmp/foodir /tmp/bardir /tmp/buzz.txt\n")
				src.WriteString("##### END")
				return src.String()
			},
			exec: func(s *script.Script) error {
				// create an executable script to apply ENV
				scriptName := "bar.sh"
				sh := "#!/bin/sh\necho $MSG1 $MSG2"
				msgExpected := "HELLO WORLD"
				if err := createTestShellScript(t, scriptName, sh); err != nil {
					return err
				}
				defer os.RemoveAll(scriptName)

				machine := s.Preambles[script.CmdFrom][0].(*script.FromCommand).Machines()[0].Host()
				workdir := s.Preambles[script.CmdWorkDir][0].(*script.WorkdirCommand)
				defer os.RemoveAll(workdir.Dir())

				// create fake files and dirs to copy
				var srcPaths []string
				for _, cmd := range []script.Command{s.Actions[1], s.Actions[3]} {
					cpCmd := cmd.(*script.CopyCommand)
					for i, path := range cpCmd.Args() {
						srcPaths = append(srcPaths, path)
						if strings.HasSuffix(path, "dir") { // create dir/file
							if err := makeTestDir(t, path); err != nil {
								return err
							}
							file := filepath.Join(path, fmt.Sprintf("file-%d.txt", i))
							if err := makeTestFakeFile(t, file, fmt.Sprintf("HelloFoo-%d", i)); err != nil {
								return err
							}
						} else { // create just file
							if err := makeTestFakeFile(t, path, "HelloFoo"); err != nil {
								return err
							}
						}
						defer os.RemoveAll(path)
					}
				}

				e := New(s)
				if err := e.Execute(); err != nil {
					return err
				}

				// validate cap cmds
				for _, cmd := range []script.Command{s.Actions[0], s.Actions[2]} {
					capCmd := cmd.(*script.CaptureCommand)
					fileName := filepath.Join(workdir.Dir(), machine, fmt.Sprintf("%s.txt", sanitizeStr(capCmd.GetCliString())))
					if _, err := os.Stat(fileName); err != nil {
						return err
					}

					if strings.HasSuffix(fileName, ".sh") {
						file, err := ioutil.ReadFile(fileName)
						if err != nil {
							return err
						}
						if strings.TrimSpace(string(file)) != msgExpected {
							return fmt.Errorf("CAPTURE ./bar.sh generated unexpected content")
						}
					}
				}

				// validate cp cmds
				for _, path := range srcPaths {
					relPath, err := filepath.Rel("/", path)
					if err != nil {
						return err
					}
					fileName := filepath.Join(workdir.Dir(), machine, relPath)
					if _, err := os.Stat(fileName); err != nil {
						return err
					}
				}

				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runExecutorTest(t, test)
		})
	}
}

// TestExecutorWithRemote test COPY command on a remote machine.
// It assumes running account has $HOME/.ssh/id_rsa private key and
// that the remote machine has public key in authorized_keys.
// If setup properly, comment out t.Skip()

func TestExecutorWithRemote(t *testing.T) {
	t.Skip(`Skipping: test requires an ssh daemon running and a
		passwordless setup using private key specified by SSHCONFIG command`)

	tests := []execTest{
		{
			name: "Executing all commands",
			source: func() string {
				src := `# This is a sample comment
				#### START
				FROM local 127.0.0.1:22
				SSHCONFIG {{.Username}}:{{.Home}}/.ssh/id_rsa
				WORKDIR /tmp/{{.Username}}
				CAPTURE echo HELLO
				COPY buzz.txt
				COPY foodir bardir buzz.txt
				##### END`
				return src
			},
			exec: func(s *script.Script) error {

				addr := "127.0.0.1:22"
				workdir := s.Preambles[script.CmdWorkDir][0].(*script.WorkdirCommand)
				defer os.RemoveAll(workdir.Dir())

				// create fake files and dirs to copy
				var srcPaths []string
				for _, cmd := range []script.Command{s.Actions[1], s.Actions[2]} {
					cpCmd := cmd.(*script.CopyCommand)
					for i, path := range cpCmd.Args() {
						srcPaths = append(srcPaths, path)
						if strings.HasSuffix(path, "dir") { // create dir/file

							t.Log("Creating local test files")
							if err := makeTestDir(t, path); err != nil {
								return err
							}
							file := filepath.Join(path, fmt.Sprintf("file-%d.txt", i))
							if err := makeTestFakeFile(t, file, fmt.Sprintf("HelloFoo-%d", i)); err != nil {
								return err
							}
							defer os.RemoveAll(path)

							t.Log("Creating remote test files")
							if err := makeRemoteTestDir(t, addr, path); err != nil {
								return err
							}
							file = filepath.Join(path, fmt.Sprintf("file-%d.txt", i))
							if err := makeRemoteTestFile(t, addr, file, fmt.Sprintf("HelloFoo-%d", i)); err != nil {
								return err
							}
							defer removeRemoteTestFile(t, addr, file)

						} else {
							if err := makeTestFakeFile(t, path, "HelloFoo"); err != nil {
								return err
							}
							defer os.RemoveAll(path)

							if err := makeRemoteTestFile(t, addr, path, "HelloFooRemote"); err != nil {
								return err
							}
							defer removeRemoteTestFile(t, addr, path)
						}
					}
				}

				e := New(s)
				if err := e.Execute(); err != nil {
					return err
				}

				// validate cap cmds
				for _, fromAddr := range []string{"local", "127.0.0.1:22"} {
					for _, cmd := range []script.Command{s.Actions[0]} {
						capCmd := cmd.(*script.CaptureCommand)
						fileName := filepath.Join(workdir.Dir(), sanitizeStr(fromAddr), fmt.Sprintf("%s.txt", sanitizeStr(capCmd.GetCliString())))
						if _, err := os.Stat(fileName); err != nil {
							return err
						}
					}
				}

				// validate cp cmds
				for _, fromAddr := range []string{"local", "127.0.0.1:22"} {
					for _, path := range srcPaths {
						fileName := filepath.Join(workdir.Dir(), sanitizeStr(fromAddr), path)
						if _, err := os.Stat(fileName); err != nil {
							return err
						}
					}
				}

				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runExecutorTest(t, test)
		})
	}
}