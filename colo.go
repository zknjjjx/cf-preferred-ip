package main

import "strings"

// 地区别名 -> Cloudflare 数据中心（IATA 机场码）集合。
// -colo 既可填机场码（LAX,SJC），也可填国家/地区别名（US、美国、JP…），可混写逗号分隔。
var coloAliases = map[string][]string{
	"US":   {"ATL", "AUS", "BOS", "BUF", "CLT", "ORD", "CMH", "DFW", "DEN", "DTW", "HNL", "IAH", "IND", "JAX", "MCI", "LAS", "LAX", "MEM", "MIA", "MSP", "BNA", "EWR", "JFK", "LGA", "OMA", "ONT", "MCO", "PDX", "PHX", "PIT", "SLC", "SAT", "SAN", "SFO", "SJC", "SEA", "STL", "TPA", "IAD"},
	"USA":  {"ATL", "AUS", "BOS", "BUF", "CLT", "ORD", "CMH", "DFW", "DEN", "DTW", "HNL", "IAH", "IND", "JAX", "MCI", "LAS", "LAX", "MEM", "MIA", "MSP", "BNA", "EWR", "JFK", "LGA", "OMA", "ONT", "MCO", "PDX", "PHX", "PIT", "SLC", "SAT", "SAN", "SFO", "SJC", "SEA", "STL", "TPA", "IAD"},
	"美国":   {"ATL", "AUS", "BOS", "BUF", "CLT", "ORD", "CMH", "DFW", "DEN", "DTW", "HNL", "IAH", "IND", "JAX", "MCI", "LAS", "LAX", "MEM", "MIA", "MSP", "BNA", "EWR", "JFK", "LGA", "OMA", "ONT", "MCO", "PDX", "PHX", "PIT", "SLC", "SAT", "SAN", "SFO", "SJC", "SEA", "STL", "TPA", "IAD"},
	"JP":   {"NRT", "HND", "KIX", "NGO", "FUK", "CTS", "OKA"},
	"日本":   {"NRT", "HND", "KIX", "NGO", "FUK", "CTS", "OKA"},
	"HK":   {"HKG"},
	"香港":   {"HKG"},
	"SG":   {"SIN"},
	"新加坡":  {"SIN"},
	"KR":   {"ICN", "GMP", "PUS"},
	"韩国":   {"ICN", "GMP", "PUS"},
	"TW":   {"TPE", "KHH"},
	"台湾":   {"TPE", "KHH"},
	"DE":   {"FRA", "MUC", "HAM", "DUS", "BER", "STR"},
	"德国":   {"FRA", "MUC", "HAM", "DUS", "BER", "STR"},
	"GB":   {"LHR", "MAN", "EDI"},
	"UK":   {"LHR", "MAN", "EDI"},
	"英国":   {"LHR", "MAN", "EDI"},
	"FR":   {"CDG", "MRS", "LYS"},
	"法国":   {"CDG", "MRS", "LYS"},
	"CA":   {"YYZ", "YVR", "YUL", "YYC"},
	"加拿大":  {"YYZ", "YVR", "YUL", "YYC"},
	"AU":   {"SYD", "MEL", "BNE", "PER", "ADL"},
	"澳大利亚": {"SYD", "MEL", "BNE", "PER", "ADL"},
	"IN":   {"DEL", "BOM", "MAA", "BLR", "HYD", "CCU"},
	"印度":   {"DEL", "BOM", "MAA", "BLR", "HYD", "CCU"},
	"NL":   {"AMS", "RTM"},
	"荷兰":   {"AMS", "RTM"},
	"SE":   {"ARN"},
	"瑞典":   {"ARN"},
}

// parseColoSpec 把 -colo 参数展开成机场码集合；空字符串返回 nil（不过滤）。
func parseColoSpec(spec string) map[string]bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	set := map[string]bool{}
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if codes, ok := coloAliases[tok]; ok {
			for _, cc := range codes {
				set[cc] = true
			}
			continue
		}
		if codes, ok := coloAliases[strings.ToUpper(tok)]; ok {
			for _, cc := range codes {
				set[cc] = true
			}
			continue
		}
		// 当作单个机场码
		set[strings.ToUpper(tok)] = true
	}
	return set
}
