package navdata

// Small fixtures in the exact column layout of the real files (verified
// against the live downloads in November 2026).

const fixAirports = `"id","ident","type","name","latitude_deg","longitude_deg","elevation_ft","continent","iso_country","iso_region","municipality","scheduled_service","icao_code","iata_code","gps_code","local_code","home_link","wikipedia_link","keywords"
2434,"EGLL","large_airport","London Heathrow Airport",51.4706,-0.461941,83,"EU","GB","GB-ENG","London","yes","EGLL","LHR","EGLL",,"https://www.heathrow.com/","https://en.wikipedia.org/wiki/Heathrow_Airport","LON, Londres"
2429,"EGKK","large_airport","London Gatwick Airport",51.148102,-0.190278,202,"EU","GB","GB-ENG","London","yes","EGKK","LGW","EGKK",,,,
2513,"EGSS","large_airport","London Stansted Airport",51.885,0.235,348,"EU","GB","GB-ENG","London","yes","EGSS","STN","EGSS",,,,
2420,"EGGW","medium_airport","London Luton Airport",51.874698638916,-0.368333011865616,526,"EU","GB","GB-ENG","Luton","yes","EGGW","LTN","EGGW",,,,
3622,"KJFK","large_airport","John F Kennedy International Airport",40.639447,-73.779317,13,"NA","US","US-NY","New York","yes","KJFK","JFK","KJFK","JFK",,,
5000,"EGLW","heliport","London Heliport",51.469793,-0.179318,18,"EU","GB","GB-ENG","London","no","EGLW",,"EGLW",,,,
5001,"XXBAD","small_airport","Broken coordinates",,-0.1,10,"EU","GB","GB-ENG",,"no",,,,,,,
5002,"NZAA","large_airport","Auckland International Airport",-37.008056,174.791667,23,"OC","NZ","NZ-AUK","Auckland","yes","NZAA","AKL","NZAA",,,,
5003,"PHNL","large_airport","Daniel K Inouye International Airport",21.32062,-157.924228,13,"OC","US","US-HI","Honolulu","yes","PHNL","HNL","PHNL",,,,
`

const fixRunways = `"id","airport_ref","airport_ident","length_ft","width_ft","surface","lighted","closed","le_ident","le_latitude_deg","le_longitude_deg","le_elevation_ft","le_heading_degT","le_displaced_threshold_ft","he_ident","he_latitude_deg","he_longitude_deg","he_elevation_ft","he_heading_degT","he_displaced_threshold_ft"
236236,2434,"EGLL",12799,164,"ASP",1,0,"09L",51.4775,-0.484981,79,89.6,1007,"27R",51.4777,-0.433322,78,269.6,
236237,2434,"EGLL",12008,164,"ASP",1,0,"09R",51.4647,-0.48223,75,89.6,,"27L",51.4649,-0.434115,77,269.6,1007
236238,2429,"EGKK",10879,148,"ASP",1,0,"08R",51.1449,-0.2095,196,77.5,,"26L",51.1514,-0.1622,202,257.5,
236239,2429,"EGKK",8415,148,"ASP",0,1,"08L",51.1468,-0.2034,,77.5,,"26R",51.1518,-0.1673,,257.5,
999999,5000,"EGLW",80,80,"CON",1,0,"H1",,,,,,,,,,,
1000000,,"BROKEN",1,1,"X",0,0,"01",,,,,,"19",,,,,
`

const fixNavaids = `"id","filename","ident","name","type","frequency_khz","latitude_deg","longitude_deg","elevation_ft","iso_country","dme_frequency_khz","dme_channel","dme_latitude_deg","dme_longitude_deg","dme_elevation_ft","slaved_variation_deg","magnetic_variation_deg","usageType","power","associated_airport"
90001,"Lambourne_VOR_GB","LAM","Lambourne","VOR-DME",115600,51.646,0.151667,190,"GB",,"103X",,,,,-1.0,"BOTH","HIGH",
90002,"Biggin_VOR_GB","BIG","Biggin","VOR-DME",115100,51.330833,0.0325,600,"GB",,"098X",,,,,-0.9,"BOTH","HIGH",
90003,"London_NDB_GB","LON","London","NDB",,999,0,,"GB",,,,,,,,"LO","LOW",
`

const fixCountries = `"id","code","name","continent","wikipedia_link","keywords"
302721,"GB","United Kingdom","EU","https://en.wikipedia.org/wiki/United_Kingdom","Great Britain"
302755,"US","United States","NA","https://en.wikipedia.org/wiki/United_States","America"
302800,"NZ","New Zealand","OC","",""
`

// Routes file with a UTF-8 BOM like the standing-data files.
const fixRoutesBAW = "\xef\xbb\xbfCallsign,Code,Number,AirlineCode,AirportCodes\n" +
	"BAW1,BAW,1,BAW,EGLL-KJFK\n" +
	"BAW117,BAW,117,BAW,EGLL-KJFK\n" +
	"BAW9X,BAW,9X,BAW,EGLL-EGSS-EGKK\n" +
	"BAWBAD,BAW,0,BAW,EGLL\n" +
	"BAW?,BAW,1,BAW,EGLL-KJFK\n"

const fixRoutesEZY = "\xef\xbb\xbfCallsign,Code,Number,AirlineCode,AirportCodes\n" +
	"EZY12AB,EZY,12AB,EZY,EGGW-EGKK\n" +
	"ANZ1,ANZ,1,ANZ,NZAA-PHNL\n"
