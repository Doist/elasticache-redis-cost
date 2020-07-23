// TODO describe program
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

func main() {
	flag.Parse()
	if err := run(flag.Arg(0)); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

func run(outputFile string) error {
	resp, err := http.Get("https://docs.aws.amazon.com/AmazonElastiCache/latest/red-ug/ParameterGroups.Redis.html")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unsupported status: %q", resp.Status)
	}
	rd, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	doc, err := html.Parse(rd)
	if err != nil {
		return err
	}
	info, err := processDoc(doc)
	if err != nil {
		return err
	}
	if len(info) == 0 {
		return fmt.Errorf("failed to parse anything useful, make sure table with id %q is present in html", tableID)
	}
	if outputFile == "" {
		for _, n := range info {
			fmt.Println(n)
		}
		return nil
	}
	m := make(map[string]uint64, len(info))
	for _, n := range info {
		m[n.Name] = n.Maxmemory
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, templateFormat, m)

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return err
	}
	return ioutil.WriteFile(outputFile, formatted, 0666)
}

const tableID = "w290aac18c46c51c49b7"

func processDoc(doc *html.Node) ([]nodeInfo, error) {
	var out []nodeInfo
	var err error
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Table {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == tableID {
					out, err = processTable(n)
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return out, err
}

func processTable(table *html.Node) ([]nodeInfo, error) {
	// Track what table column we're in. Reset to 0 each time we enter new row,
	// increase by one on each th or td.
	var column int
	// column indexes derived from header
	var instanceTypeColumn, maxmemoryColumn int
	var hasInstanceTypeColumn, hasMaxmemoryColumn bool
	var seenHeader bool
	var f func(*html.Node)

	var out []nodeInfo
	var elastiCacheNode nodeInfo // reset on each row

	var err error
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Tr {
			elastiCacheNode = nodeInfo{}
			column = 0
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Th {
			switch text := strings.TrimSpace(nodeText(n)); {
			case strings.EqualFold(text, "Node Type"):
				instanceTypeColumn = column
				hasInstanceTypeColumn = true
			case strings.EqualFold(text, "maxmemory"):
				maxmemoryColumn = column
				hasMaxmemoryColumn = true
			}
			seenHeader = hasMaxmemoryColumn && hasInstanceTypeColumn
			column++
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Td {
			if seenHeader && (column == instanceTypeColumn || column == maxmemoryColumn) {
				text := strings.TrimSpace(nodeText(n))
				switch column {
				case instanceTypeColumn:
					elastiCacheNode.Name = text
				case maxmemoryColumn:
					var mem uint64
					mem, err = strconv.ParseUint(text, 10, 64)
					if err != nil {
						return
					}
					elastiCacheNode.Maxmemory = mem
				}
				if elastiCacheNode.Name != "" && elastiCacheNode.Maxmemory != 0 {
					out = append(out, elastiCacheNode)
				}
			}
			column++
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(table)
	return out, err
}

type nodeInfo struct {
	Name      string
	Maxmemory uint64
}

// nodeText returns text extracted from node and all its descendants
func nodeText(n *html.Node) string {
	var b strings.Builder
	var fn func(*html.Node)
	fn = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			fn(c)
		}
	}
	fn(n)
	return b.String()
}

const templateFormat = `// Code generated by parse-maxmemory; DO NOT EDIT.

package main

var maxmemoryValues = %#v
`
