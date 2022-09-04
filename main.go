package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows/registry"
)

const (
	ETS = `Euro Truck Simulator 2`
	ATS = `American Truck Simulator`
)

var (
	profileList   []string
	watchPathList []string
)

func main() {
	if !isFile("SII_Decrypt.exe") {
		handleError(errors.New("SII_Decrypt.exe不存在！"))
	}

	err := addDocumentsPathToWatchList()
	handleError(err)
	addProfilePathToWatchList()

	fmt.Println("================= TPC For TruckersMP =================")
	fmt.Println("Usage: 0.Type g_debug_camera 1 in console (only once)")
	fmt.Println("       1.Alt+F12 to save coordinate of freecam")
	fmt.Println("       2.Make a quicksave & reload 5 seconds later")
	fmt.Println("Email: sunwinbus@ets666.com | Discord: sunwinbus#1343")
	fmt.Println("Thanks to DF-41 for his great idea!")
	fmt.Println("======================================================")

	watch, err := fsnotify.NewWatcher()
	handleError(err)
	defer watch.Close()

	err = addPathToWatch(watch)
	handleError(err)

	go watchQuicksave(watch)
	select {}
}

func handleError(err error) {
	if err != nil {
		fmt.Println("Fatal error, stop working!")
		fmt.Println(err)
		bufio.NewReader(os.Stdin).ReadBytes('\n')
		os.Exit(0)
	}
}

//从注册表读取我的文档路径
func getDocumentsPath() (string, error) {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, "Software\\Microsoft\\Windows\\CurrentVersion\\Explorer\\User Shell Folders", registry.ALL_ACCESS)
	if err != nil {
		return "", err
	}
	defer key.Close()
	path, _, _ := key.GetStringValue("Personal")
	path = strings.TrimSpace(strings.Replace(path, "%USERPROFILE%", os.Getenv("USERPROFILE"), -1))
	return path, nil
}

// 检索文档目录是否有游戏配置文件
func addDocumentsPathToWatchList() error {
	documentsPath, err := getDocumentsPath()
	if err != nil {
		return err
	}
	if ets2ProfilePath := filepath.Join(documentsPath, ETS, `profiles`); isDir(ets2ProfilePath) {
		err = getProfileList(ets2ProfilePath)
		if err != nil {
			return err
		}
		watchPathList = append(watchPathList, ets2ProfilePath)
	}
	if atsProfilePath := filepath.Join(documentsPath, ATS, `profiles`); isDir(atsProfilePath) {
		err = getProfileList(atsProfilePath)
		if err != nil {
			return err
		}
		watchPathList = append(watchPathList, atsProfilePath)
	}
	return nil
}

func addProfilePathToWatchList() {
	for _, profilePath := range profileList {
		if isDir(filepath.Join(profilePath, `save`)) {
			watchPathList = append(watchPathList, filepath.Join(profilePath, `save`))
		} else {
			watchPathList = append(watchPathList, profilePath)
		}
	}
	profileList = profileList[0:0]
}

func listProfiles(path string, f os.FileInfo, err error) error {
	if f == nil {
		return err
	}
	if f.IsDir() && filepath.Base(filepath.Dir(path)) == `profiles` {
		profileList = append(profileList, path)
	}
	return nil
}

func getProfileList(path string) error {
	err := filepath.Walk(path, listProfiles)
	if err != nil {
		return err
	}
	return nil
}

func isFile(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	if s.IsDir() {
		return false
	}
	return true
}

func isDir(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	if s.IsDir() {
		return true
	}
	return false
}

func decryptSii(filePath string) (bool, error) {
	cmd := exec.Command("SII_Decrypt.exe", filePath)
	buf, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.Sys().(syscall.WaitStatus).ExitStatus() == 1 {
				return false, nil
			}
			return false, errors.New(string(buf))
		}
		return false, errors.New(string(buf))
	}
	return true, nil
}

func readFile(filePath string) ([]string, error) {
	f, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)
	output := make([]string, 0)
	for scanner.Scan() {
		output = append(output, scanner.Text())
	}
	return output, nil
}

func writeFile(filePath string, outPut string) error {
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := bufio.NewWriter(f)
	_, err = writer.WriteString(outPut)
	if err != nil {
		return err
	}
	writer.Flush()
	return nil
}

func addPathToWatch(watch *fsnotify.Watcher) error {
	for _, watchPath := range watchPathList {
		err := watch.Add(watchPath)
		if err != nil {
			return err
		}
		fmt.Println("Monitoring: " + watchPath)
	}
	return nil
}

func watchQuicksave(watch *fsnotify.Watcher) {
	for {
		select {
		case ev := <-watch.Events:
			{
				if ev.Op&fsnotify.Create == fsnotify.Create {
					if filepath.Base(ev.Name) == `quicksave` {
						time.Sleep(3 * time.Second)
						done, err := flushChange(filepath.Join(ev.Name, `game.sii`))
						handleError(err)
						if done {
							fmt.Println("Updated: " + filepath.Join(ev.Name, `game.sii`))
						}
					} else if filepath.Base(ev.Name) == `profiles` && filepath.Base(filepath.Dir(ev.Name)) == `remote` {
						err := watch.Add(ev.Name)
						handleError(err)
						fmt.Println("Monitoring: " + ev.Name)
						err = getProfileList(ev.Name)
						handleError(err)
						for _, profilePath := range profileList {
							err = watch.Add(profilePath)
							handleError(err)
						}
					} else if isDir(ev.Name) && filepath.Base(filepath.Dir(ev.Name)) == `profiles` {
						err := watch.Add(ev.Name)
						handleError(err)
					} else if filepath.Base(ev.Name) == `save` {
						err := watch.Add(ev.Name)
						handleError(err)
						fmt.Println("Monitoring: " + ev.Name)
					}
				}
				if ev.Op&fsnotify.Write == fsnotify.Write {
					if filepath.Base(ev.Name) == `quicksave` {
						time.Sleep(3 * time.Second)
						done, err := flushChange(filepath.Join(ev.Name, `game.sii`))
						handleError(err)
						if done {
							fmt.Println("Updated: " + filepath.Join(ev.Name, `game.sii`))
						}
					}
				}
			}
		case err := <-watch.Errors:
			{
				handleError(err)
			}
		}
	}
}

func flushChange(filePath string) (bool, error) {
	if !isFile(filePath) {
		return false, nil
	}
	needEdit, err := decryptSii(filePath)
	if err != nil {
		return false, err
	}
	if !needEdit {
		return false, nil
	}

	if !isFile(filePath) {
		return false, nil
	}
	sii, err := readFile(filePath)
	if err != nil {
		return false, err
	}

	documentsPath, err := getDocumentsPath()
	if err != nil {
		return false, err
	}
	camsPath := filepath.Join(documentsPath, ETS, `cams.txt`)
	if strings.Contains(filePath, ATS) {
		camsPath = filepath.Join(documentsPath, ATS, `cams.txt`)
	}
	if !isFile(camsPath) {
		return false, nil
	}
	cams, err := readFile(camsPath)
	if err != nil {
		return false, err
	}

	if len(cams) > 0 {
		location, rotation := parseCamsCoordinate(cams)
		if err != nil {
			return false, err
		}

		output, err := editSii(sii, location, rotation)
		if err != nil {
			return false, err
		}

		if !isFile(filePath) {
			return false, nil
		}
		err = writeFile(filePath, output)
		if err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

func parseCamsCoordinate(cams []string) (string, string) {
	camCoordinate := strings.ReplaceAll(cams[len(cams)-1], `;`, `,`)
	location := strings.Split(camCoordinate, ` , `)[1]
	rotation := strings.Split(camCoordinate, ` , `)[2]
	rotation = strings.Replace(rotation, `,`, `;`, 1)
	fmt.Println("Target:" + `(` + location + `) (` + rotation + `)`)
	return location, rotation
}

func editSii(siiArray []string, location string, rotation string) (string, error) {
	for i := range siiArray {
		if strings.HasPrefix(siiArray[i], " truck_placement:") {
			siiArray[i] = " truck_placement: " + `(` + location + `) (` + rotation + `)`
		} else if strings.HasPrefix(siiArray[i], " trailer_placement:") {
			siiArray[i] = ` trailer_placement: (0, 0, 0) (` + rotation + `)`
		} else if strings.HasPrefix(siiArray[i], " slave_trailer_placements[") {
			siiArray[i] = strings.Split(siiArray[i], `: `)[0] + `: (0, 0, 0) (` + rotation + `)`
		} else if strings.HasPrefix(siiArray[i], " trailer_body_wear:") {
			siiArray[i] = " trailer_body_wear: 0"
		} else if strings.HasPrefix(siiArray[i], " chassis_wear:") {
			siiArray[i] = " chassis_wear: 0"
		} else if strings.HasPrefix(siiArray[i], " engine_wear:") {
			siiArray[i] = " engine_wear: 0"
		} else if strings.HasPrefix(siiArray[i], " transmission_wear:") {
			siiArray[i] = " transmission_wear: 0"
		} else if strings.HasPrefix(siiArray[i], " cabin_wear:") {
			siiArray[i] = " cabin_wear: 0"
		} else if strings.HasPrefix(siiArray[i], " wheels_wear:") {
			siiArray[i] = " wheels_wear: 0"
		} else if strings.HasPrefix(siiArray[i], " wheels_wear[") {
			siiArray[i] = ""
		} else if strings.HasPrefix(siiArray[i], " fuel_relative:") {
			siiArray[i] = " fuel_relative: 1"
		}
	}
	return strings.Join(siiArray, "\n"), nil
}
