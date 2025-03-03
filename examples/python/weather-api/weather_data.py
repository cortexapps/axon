import openmeteo_requests
from openmeteo_sdk.WeatherApiResponse import WeatherApiResponse

import requests_cache
import pandas as pd
from retry_requests import retry
from dataclasses import dataclass
from datetime import datetime
from datetime import timezone
import pytz


def load_cities(max: int = None, file_path: str = "cities.csv") -> list["City"]:

    cities_df = pd.read_csv(file_path)
    cities_df.columns = cities_df.columns.str.strip()

    cities = []

    if max > 0:
        cities_df = cities_df.head(max)

    for index, row in cities_df.iterrows():
        city = City(
            name=row["City"],
            country=row["Country"],
            lat=row["Latitude"],
            long=row["Longitude"],
            timezone=row["Timezone"],
        )
        cities.append(city)

    return cities



@dataclass
class City:
    name: str
    country: str
    lat: float
    long: float
    timezone: str

    def __init__(self, name: str, country: str, lat: float, long: float, timezone: str):
        self.name = name.strip()
        self.country = country.strip()
        self.lat = lat
        self.long = long
        self.timezone = timezone.strip()

    def continent(self) -> str:
        parts = self.timezone.split("/")
        return parts[0]

    def datetime(self, time: int) -> datetime:
        timezone = pytz.timezone(self.timezone)
        return datetime.fromtimestamp(time, timezone)


@dataclass
class CityWeather:
    city: City
    timestamp: int
    temperature: float
    humidity: float
    is_day: bool
    precipitation: float
    rain: float
    showers: float
    snowfall: float
    cloud_cover: float
    wind_speed: float
    response: WeatherApiResponse

    def __init__(self, city, response):

        self.response = response
        self.timestamp = response.Current().Time()

        # Current values. The order of variables needs to be the same as requested.
        current = response.Current()
        current_temperature_2m = current.Variables(0).Value()
        current_relative_humidity_2m = current.Variables(1).Value()
        current_is_day = current.Variables(2).Value()
        current_precipitation = current.Variables(3).Value()
        current_rain = current.Variables(4).Value()
        current_showers = current.Variables(5).Value()
        current_snowfall = current.Variables(6).Value()
        # current_weather_code = current.Variables(7).Value()
        current_cloud_cover = current.Variables(8).Value()
        current_wind_speed_10m = current.Variables(9).Value()
        # current_wind_direction_10m = current.Variables(10).Value()

        self.city = city
        self.temperature = current_temperature_2m
        self.humidity = current_relative_humidity_2m
        self.is_day = current_is_day
        self.precipitation = current_precipitation
        self.rain = current_rain
        self.showers = current_showers
        self.snowfall = current_snowfall
        self.cloud_cover = current_cloud_cover
        self.wind_speed = current_wind_speed_10m

    def datetime(self) -> datetime:
        return self.city.datetime(self.timestamp)

    def dump(self):

        response = self.response
        city = self.city

        # Current values. The order of variables needs to be the same as requested.
        current = response.Current()
        current_temperature_2m = current.Variables(0).Value()
        current_relative_humidity_2m = current.Variables(1).Value()
        current_is_day = current.Variables(2).Value()
        current_precipitation = current.Variables(3).Value()
        current_rain = current.Variables(4).Value()
        current_showers = current.Variables(5).Value()
        current_snowfall = current.Variables(6).Value()
        current_weather_code = current.Variables(7).Value()
        current_cloud_cover = current.Variables(8).Value()
        current_wind_speed_10m = current.Variables(9).Value()
        current_wind_direction_10m = current.Variables(10).Value()

        print(f"City: {self.city.name}")
        print(f"Coordinates {response.Latitude()}°N {response.Longitude()}°E")
        print(f"Elevation {response.Elevation()} m asl")
        print(f"Timezone {response.Timezone()} {response.TimezoneAbbreviation()}")
        print(f"Timezone difference to GMT+0 {response.UtcOffsetSeconds()} s")

        print(f"UTC time {datetime.fromtimestamp(current.Time(), timezone.utc)} UTC")
        print(f"City local time {city.datetime(current.Time())}")
        print(f"Current temperature_2m {current_temperature_2m}")
        print(f"Current relative_humidity_2m {current_relative_humidity_2m}")
        print(f"Current is_day {current_is_day}")
        print(f"Current precipitation {current_precipitation}")
        print(f"Current rain {current_rain}")
        print(f"Current showers {current_showers}")
        print(f"Current snowfall {current_snowfall}")
        print(f"Current weather_code {current_weather_code}")
        print(f"Current cloud_cover {current_cloud_cover}")
        print(f"Current wind_speed_10m {current_wind_speed_10m}")
        print(f"Current wind_direction_10m {current_wind_direction_10m}")


class WeatherData:

    def __init__(self):
        cache_session = requests_cache.CachedSession(".cache", expire_after=1800)
        retry_session = retry(cache_session, retries=5, backoff_factor=0.2)
        self.openmeteo = openmeteo_requests.Client(session=retry_session)

    def fetch(self, cities: list["City"]) -> list["CityWeather"]:
        return self.__get_weather_data(cities)

    def __get_weather_data(
        self, cities: list["City"], openmeteo: openmeteo_requests.Client = None
    ):

        if openmeteo is None:
            openmeteo = self.openmeteo

        # Make sure all required weather variables are listed here
        # The order of variables in hourly or daily is important to assign them correctly below
        url = "https://api.open-meteo.com/v1/forecast"
        params = {
            "latitude": [city.lat for city in cities],
            "longitude": [city.long for city in cities],
            "timezone": [city.timezone for city in cities],
            "current": [
                "temperature_2m",
                "relative_humidity_2m",
                "is_day",
                "precipitation",
                "rain",
                "showers",
                "snowfall",
                "weather_code",
                "cloud_cover",
                "wind_speed_10m",
                "wind_direction_10m",
            ],
            "forecast_days": 3,
        }
        responses = openmeteo.weather_api(url, params=params)

        # Process first location. Add a for-loop for multiple locations or weather models
        results = []
        for index, response in enumerate(responses):
            result = self.__process_response(cities[index], response)
            results.append(result)

        return results

    def __process_response(self, city: "City", response):

        city_weather = CityWeather(city, response)

        return city_weather

