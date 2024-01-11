package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sagernet/sing-box/common/srs"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

var token string

func init() {
	ipinfoToken, loaded := os.LookupEnv("IPINFO_TOKEN")
	if !loaded {
		panic("no IPINFO_TOKEN")
	}
	token = ipinfoToken
}

func get(downloadURL string) ([]byte, error) {
	log.Info("download ", downloadURL)
	response, err := http.Get(downloadURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func download() ([]byte, error) {
	url := fmt.Sprintf("https://ipinfo.io/data/free/country.mmdb?token=%s", token)
	return get(url)
}

type IpinfoContry struct {
	Continent         string            `maxminddb:"continent"`
	ContinentName     string            `maxminddb:"continent_name"`
	Country           string            `maxminddb:"country"`
	CountryName       string            `maxminddb:"country_name"`
}

func parse(binary []byte) (metadata maxminddb.Metadata, countryMap map[string][]*net.IPNet, err error) {
	database, err := maxminddb.FromBytes(binary)
	if err != nil {
		return
	}
	metadata = database.Metadata
	networks := database.Networks(maxminddb.SkipAliasedNetworks)
	countryMap = make(map[string][]*net.IPNet)
	var country IpinfoContry
	var ipNet *net.IPNet
	for networks.Next() {
		ipNet, err = networks.Network(&country)
		if err != nil {
			return
		}
		var code string
		if country.Country != "" {
			code = strings.ToLower(country.Country)
		} else if country.Continent != "" {
			code = strings.ToLower(country.Continent)
		} else {
			continue
		}
		countryMap[code] = append(countryMap[code], ipNet)
	}
	err = networks.Err()
	return
}

func newWriter(metadata maxminddb.Metadata, codes []string) (*mmdbwriter.Tree, error) {
	return mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "sing-geoip",
		Languages:               codes,
		IPVersion:               int(metadata.IPVersion),
		RecordSize:              int(metadata.RecordSize),
		Inserter:                inserter.ReplaceWith,
		DisableIPv4Aliasing:     true,
		IncludeReservedNetworks: true,
	})
}

func open(path string, codes []string) (*mmdbwriter.Tree, error) {
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	if reader.Metadata.DatabaseType != "sing-geoip" {
		return nil, E.New("invalid sing-geoip database")
	}
	reader.Close()

	return mmdbwriter.Load(path, mmdbwriter.Options{
		Languages: append(reader.Metadata.Languages, common.Filter(codes, func(it string) bool {
			return !common.Contains(reader.Metadata.Languages, it)
		})...),
		Inserter: inserter.ReplaceWith,
	})
}

func write(writer *mmdbwriter.Tree, dataMap map[string][]*net.IPNet, output string, codes []string) error {
	if len(codes) == 0 {
		codes = make([]string, 0, len(dataMap))
		for code := range dataMap {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	codeMap := make(map[string]bool)
	for _, code := range codes {
		codeMap[code] = true
	}
	for code, data := range dataMap {
		if !codeMap[code] {
			continue
		}
		for _, item := range data {
			err := writer.Insert(item, mmdbtype.String(code))
			if err != nil {
				return err
			}
		}
	}
	outputFile, err := os.Create(output)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	_, err = writer.WriteTo(outputFile)
	return err
}

func release(output string, ruleSetOutput string) error {
	binary, err := download()
	if err != nil {
		return err
	}
	metadata, countryMap, err := parse(binary)
	if err != nil {
		return err
	}
	allCodes := make([]string, 0, len(countryMap))
	for code := range countryMap {
		allCodes = append(allCodes, code)
	}

	writer, err := newWriter(metadata, allCodes)
	if err != nil {
		return err
	}
	err = write(writer, countryMap, output, nil)
	if err != nil {
		return err
	}

	writer, err = newWriter(metadata, []string{"cn"})
	if err != nil {
		return err
	}
	err = write(writer, countryMap, "geoip-cn.db", []string{"cn"})
	if err != nil {
		return err
	}

	os.RemoveAll(ruleSetOutput)
	err = os.MkdirAll(ruleSetOutput, 0o755)
	if err != nil {
		return err
	}
	for countryCode, ipNets := range countryMap {
		var headlessRule option.DefaultHeadlessRule
		headlessRule.IPCIDR = make([]string, 0, len(ipNets))
		for _, cidr := range ipNets {
			headlessRule.IPCIDR = append(headlessRule.IPCIDR, cidr.String())
		}
		var plainRuleSet option.PlainRuleSet
		plainRuleSet.Rules = []option.HeadlessRule{
			{
				Type:           C.RuleTypeDefault,
				DefaultOptions: headlessRule,
			},
		}
		srsPath, _ := filepath.Abs(filepath.Join(ruleSetOutput, "geoip-"+countryCode+".srs"))
		os.Stderr.WriteString("write " + srsPath + "\n")
		outputRuleSet, err := os.Create(srsPath)
		if err != nil {
			return err
		}
		err = srs.Write(outputRuleSet, plainRuleSet)
		if err != nil {
			outputRuleSet.Close()
			return err
		}
		outputRuleSet.Close()
	}

	setActionOutput("tag", "ipinfo")
	return nil
}

func setActionOutput(name string, content string) {
	os.Stdout.WriteString("::set-output name=" + name + "::" + content + "\n")
}

func main() {
	err := release("geoip.db", "rule-set")
	if err != nil {
		log.Fatal(err)
	}
}
