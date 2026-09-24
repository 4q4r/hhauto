package hhapi

import "hhauto/internal/config"

// epsCfg builds endpoint config used by unit tests (no live network).
func epsCfg() config.APIEndpoints {
	return config.APIEndpoints{
		Site:       "https://hh.ru",
		Chatik:     "https://chatik.hh.ru",
		SearchPage: "https://hh.ru/search/vacancy",
		Override: map[string]string{
			epResumes: "https://mirror.example.ru/shards/applicant/resumes",
		},
	}
}
