package webhook

var mappers = []Mapper{
	Map("sonarr", fromSonarr),
	Map("radarr", fromRadarr),
}
