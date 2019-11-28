package serverstats

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/retryableredis"
	"github.com/jonas747/yagpdb/bot/botrest"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/serverstats/models"
	"github.com/sirupsen/logrus"
	"github.com/volatiletech/sqlboiler/queries/qm"
)

func RetrieveDailyStatsLegacy(guildID int64) (*DailyStats, error) {
	if os.Getenv("YAGPDB_SERVERSTATS_DISABLE_SERVERSTATS") != "" {
		return &DailyStats{}, nil
	}

	// Query the short term stats and the long term stats
	// TODO: If we start moving them over in between we will get somehwat incorrect stats
	// not sure how to fix other than locking

	stats, err := RetrieveRedisStatsLegacy(guildID)
	if err != nil {
		return nil, err
	}

	// rows, err := ServerStatsPeriodStore.FindAll(models.NewStatsPeriodQuery().FindByGuildID(
	// 	kallax.Eq, guildID).FindByStarted(kallax.Gt))

	messageStatsRows, err := models.ServerStatsPeriods(qm.Where("guild_id = ?", guildID), qm.Where("started > ?", time.Now().Add(time.Hour*-24))).AllG(context.Background())
	if err != nil {
		return nil, err
	}

	// Merge the stats togheter
	for _, period := range messageStatsRows {
		stringedChannel := strconv.FormatInt(period.ChannelID.Int64, 10)
		if st, ok := stats.ChannelMessages[stringedChannel]; ok {
			st.Count += period.Count.Int64
		} else {
			stats.ChannelMessages[stringedChannel] = &ChannelStats{
				Name:  stringedChannel,
				Count: period.Count.Int64,
			}
		}
	}

	t := RoundHour(time.Now())
	memberStatsRows, err := models.ServerStatsMemberPeriods(
		models.ServerStatsMemberPeriodWhere.GuildID.EQ(guildID),
		qm.Where("created_at > ?", time.Now().Add(time.Hour*-25))).AllG(context.Background())
	if err != nil {
		return nil, err
	}

	// Sum the stats
	for i, v := range memberStatsRows {
		if i == 0 {
			stats.TotalMembers = int(v.NumMembers)
			if v.CreatedAt.UTC() == t.UTC() {
				continue
			}
		}

		if t.Sub(v.CreatedAt) > time.Hour*25 {
			break
		}

		stats.JoinedDay += int(v.Joins)
		stats.LeftDay += int(v.Leaves)
	}

	return stats, nil
}

func RetrieveRedisStatsLegacy(guildID int64) (*DailyStats, error) {
	now := time.Now()
	yesterday := now.Add(time.Hour * -24)
	unixYesterday := discordgo.StrID(yesterday.Unix())

	var messageStatsRaw []string

	err := common.RedisPool.Do(retryableredis.Cmd(&messageStatsRaw, "ZRANGEBYSCORE", RedisKeyChannelMessages(guildID), unixYesterday, "+inf"))
	if err != nil {
		return nil, err
	}

	online, err := botrest.GetOnlineCount(guildID)
	if err != nil {
		logger.WithError(err).Error("Failed fetching online count")
	}

	channelResult, err := parseMessageStatsLegacy(messageStatsRaw, guildID)
	if err != nil {
		return nil, err
	}

	stats := &DailyStats{
		ChannelMessages: channelResult,
		Online:          int(online),
	}

	return stats, nil
}

func parseMessageStatsLegacy(raw []string, guildID int64) (map[string]*ChannelStats, error) {

	channelResult := make(map[string]*ChannelStats)
	for _, result := range raw {
		split := strings.Split(result, ":")
		if len(split) < 2 {

			logger.WithFields(logrus.Fields{
				"guild":  guildID,
				"result": result,
			}).Error("Invalid message stats")

			continue
		}
		channelID := split[0]

		stats, ok := channelResult[channelID]
		if ok {
			stats.Count++
		} else {
			name := channelID
			channelResult[channelID] = &ChannelStats{
				Name:  name,
				Count: 1,
			}
		}
	}
	return channelResult, nil
}

func RetrieveMemberChartStatsLegacy(guildID int64, days int) ([]*MemberChartDataPeriod, error) {
	queryFirstHalf := `select date_trunc('day', created_at), sum(joins), sum(leaves), max(num_members), max(max_online)
FROM server_stats_member_periods
WHERE guild_id=$1`
	querySecondHalf := `
GROUP BY 1 
ORDER BY 1 DESC`

	whereTimeClause := ""

	if days > 0 {
		whereTimeClause = fmt.Sprintf("\nAND created_at > now() - INTERVAL '(%d days)'\n", days+1)
	}

	fullQuery := queryFirstHalf + whereTimeClause + querySecondHalf
	rows, err := common.PQ.Query(fullQuery, guildID)

	if err != nil {
		return nil, errors.WrapIf(err, "pq.query")
	}

	defer rows.Close()

	var results []*MemberChartDataPeriod
	if days > 0 {
		results = make([]*MemberChartDataPeriod, days)
	} else {
		// we don't know the size
		results = make([]*MemberChartDataPeriod, 100)
	}

	for rows.Next() {
		var t time.Time
		var joins int
		var leaves int
		var numMembers int
		var maxOnline int

		err := rows.Scan(&t, &joins, &leaves, &numMembers, &maxOnline)
		if err != nil {
			return nil, errors.WrapIf(err, "rows.scan")
		}

		daysOld := int(time.Since(t).Hours() / 24)

		if daysOld > days && len(results) > 0 && days > 0 {
			// only grab results within time period specified (but always grab 1 even if outside our range)
			break
		}

		if days > 0 && daysOld >= days {
			// clamp to last if we specified a time
			daysOld = days - 1
		}

		if daysOld >= len(results) {
			if daysOld > 10000 {
				continue // ignore this then, should never happen, but lets just avoid running out of memory if it does
			}

			newResults := make([]*MemberChartDataPeriod, daysOld*2)
			copy(newResults, results)
			results = newResults
		}

		results[daysOld] = &MemberChartDataPeriod{
			T:          t,
			Joins:      joins,
			Leaves:     leaves,
			NumMembers: numMembers,
			MaxOnline:  maxOnline,
		}
	}

	firstNonNullResult := -1

	// fill in the blank days
	var lastProperResult MemberChartDataPeriod
	for i := len(results) - 1; i >= 0; i-- {
		if results[i] == nil && !lastProperResult.T.IsZero() {
			cop := lastProperResult
			t := time.Now().Add(time.Hour * 24 * -time.Duration(i))
			cop.T = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, lastProperResult.T.Location())

			results[i] = &cop
		} else if results[i] != nil {
			lastProperResult = *results[i]
			lastProperResult.Joins = 0
			lastProperResult.Leaves = 0
			if firstNonNullResult == -1 {
				firstNonNullResult = i
			}
		}
	}

	// cut out nil results
	results = results[:firstNonNullResult+1]

	return results, nil
}

func RetrieveMessageChartDataLegacy(guildID int64, days int) ([]*MessageChartDataPeriod, error) {
	queryPre := `select date_trunc('day', started), sum(count)
FROM server_stats_periods
WHERE guild_id=$1 `
	queryPost := `
GROUP BY 1 
ORDER BY 1 DESC`

	args := []interface{}{guildID}
	if days > 0 {
		queryPre += " AND started > $2"
		args = append(args, time.Now().Add(time.Hour*24*time.Duration(-days)))
	}

	rows, err := common.PQ.Query(queryPre+queryPost, args...)

	if err != nil {
		return nil, errors.WrapIf(err, "pq.query")
	}

	defer rows.Close()

	var results []*MessageChartDataPeriod
	if days > 0 {
		results = make([]*MessageChartDataPeriod, days)
	} else {
		// we don't know the size
		results = make([]*MessageChartDataPeriod, 100)
	}

	for rows.Next() {
		var t time.Time
		var count int

		err := rows.Scan(&t, &count)
		if err != nil {
			return nil, errors.WrapIf(err, "rows.scan")
		}

		daysOld := int(time.Since(t).Hours() / 24)

		if daysOld >= days && days > 0 {
			// clamp to last if we specified a time
			daysOld = days - 1
		}

		if daysOld >= len(results) {
			// we don't know the size so we have to dynamically adjust
			if daysOld > 10000 {
				continue // ignore this then, should never happen, but lets just avoid running out of memory if it does
			}

			newResults := make([]*MessageChartDataPeriod, daysOld*2)
			copy(newResults, results)
			results = newResults
		}

		results[daysOld] = &MessageChartDataPeriod{
			T:            t,
			MessageCount: count,
		}
	}

	firstNonNullResult := -1

	// fill in the blank days
	var lastProperResult MessageChartDataPeriod
	for i := len(results) - 1; i >= 0; i-- {
		if results[i] == nil && !lastProperResult.T.IsZero() {
			cop := lastProperResult
			t := time.Now().Add(time.Hour * 24 * -time.Duration(i))
			cop.T = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, lastProperResult.T.Location())

			results[i] = &cop
		} else if results[i] != nil {
			lastProperResult = *results[i]
			lastProperResult.MessageCount = 0

			if firstNonNullResult == -1 {
				firstNonNullResult = i
			}
		}
	}

	// cut out nil results
	results = results[:firstNonNullResult+1]

	return results, nil
}