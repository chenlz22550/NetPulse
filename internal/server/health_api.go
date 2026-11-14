package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// MarshalJSON keeps numeric KPIs for populated reports and represents absence as null.
func (k rptKPI) MarshalJSON() ([]byte, error) {
	type plain rptKPI
	if k.HasData || k.WithData > 0 {
		if k.SuccessSamples == 0 && k.AvgMs == 0 {
			return json.Marshal(struct {
				plain
				Avg *float64 `json:"avg_ms"`
			}{plain: plain(k)})
		}
		return json.Marshal(plain(k))
	}
	return json.Marshal(struct {
		plain
		Availability *float64 `json:"availability_pct"`
		Loss         *float64 `json:"loss_pct"`
		Avg          *float64 `json:"avg_ms"`
	}{plain: plain(k)})
}

func (s *Server) generateHealthReportWithBinding(rg, wantProfileID string, wantEpoch *int64) (*Report, *rptData, string, int64, error) {
	if rg == "" {
		rg = "24h"
	}
	done := s.Topo.ProfileWork()
	defer done()
	activeID, epoch, err := s.checkActiveBindingLocked(wantProfileID, wantEpoch)
	if err != nil {
		return nil, nil, activeID, epoch, err
	}
	now := time.Now()
	d, err := s.collectReport(rg, now)
	if err != nil {
		return nil, nil, activeID, epoch, err
	}
	title := d.Site + " 机房健康" + d.Kind
	body := "# " + title + "\n\n统计方式：程序确定性汇总，无需 AI。故障次数按每台设备连续出现丢包的分钟段计数，非告警通知次数。\n"
	if d.Minutes == 0 {
		body += "\n本周期无观测数据，无法判断可用率、丢包率和延迟。\n"
	}
	if d.RetentionShort {
		body += "\n历史保留时长小于报告周期，统计不覆盖完整周期。\n"
	}
	if d.Truncated > 0 {
		body += "\n设备数量超过统计上限，此报告仅覆盖部分设备。\n"
	}
	rp := &Report{
		ID:         now.Format("20060102-150405"),
		Time:       now.Format("2006-01-02 15:04:05"),
		Range:      d.Range,
		RangeLabel: d.Label,
		Kind:       d.Kind,
		Site:       d.Site,
		From:       d.From.Format(time.RFC3339),
		To:         d.To.Format(time.RFC3339),
		Title:      title,
		Markdown:   body + d.appendix(),
		Model:      "deterministic",
		Summary:    d.kpi(),
	}
	if err := s.saveReport(rp); err != nil {
		return nil, nil, activeID, epoch, err
	}
	return rp, d, activeID, epoch, nil
}

func (s *Server) generateHealthReport(rg string) (*Report, *rptData, error) {
	rp, d, _, _, err := s.generateHealthReportWithBinding(rg, "", nil)
	return rp, d, err
}

func (s *Server) handleHealthReport(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Range string `json:"range"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	rp, _, err := s.generateHealthReport(req.Range)
	if err != nil {
		return err
	}
	writeJSON(w, rp)
	return nil
}
