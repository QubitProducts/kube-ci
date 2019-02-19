package main

import (
	"bufio"
	"io"
	"log"
	"strconv"
	"strings"

	"github.com/google/go-github/v22/github"
	"k8s.io/client-go/kubernetes"
)

// getPodLogs returns the logs for pod, the caller must close the stream
func getPodLogs(client kubernetes.Interface, name, namespace, container string) (io.ReadCloser, error) {
	log.Printf("trying to stream logs for %s/%s[%s]", name, namespace, container)
	req := client.CoreV1().RESTClient().Get().
		Namespace(namespace).
		Name(name).
		Resource("pods").
		SubResource("log").
		Param("container", container)

	readCloser, err := req.Stream()
	if err != nil {
		return nil, err
	}

	return readCloser, nil
}

func parseAnnotations(r io.ReadCloser, filePrefix string) ([]*github.CheckRunAnnotation, error) {
	log.Println("attempting to parse logs")
	defer r.Close()
	scanner := bufio.NewScanner(r)
	var anns []*github.CheckRunAnnotation

	for scanner.Scan() {
		strs := strings.SplitN(scanner.Text(), ":", 3)
		// we'll assume conventional file:line: message or file:line:col: message...
		if len(strs) < 3 {
			continue
		}

		// remove any preceding path info from the filename
		fn := strings.TrimLeft(
			strings.TrimPrefix(
				strings.TrimSpace(strs[0]),
				filePrefix),
			`./\`)

		line, err := strconv.Atoi(strs[1])
		if err != nil {
			continue
		}

		message := ""
		rest := strs[2]

		strs = strings.SplitN(rest, ":", 2)
		switch len(strs) {
		case 1:
			if len(strs[0]) == 0 || strs[0][0] != ' ' {
				continue
			}
			message = strings.TrimSpace(strs[0])
		case 2:
			if len(strs[1]) == 0 || strs[1][0] != ' ' {
				continue
			}
			message = strings.TrimSpace(strs[1])
		}

		level := "notice"
		// if we get here, we have something approaching a
		anns = append(anns, &github.CheckRunAnnotation{
			Path:            &fn,
			AnnotationLevel: &level,
			StartLine:       &line,
			EndLine:         &line,

			Message: &message,
		})
	}
	log.Printf("Found %d annotations", len(anns))

	return anns, scanner.Err()
}
