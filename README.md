# Air

A Go application to read PM2.5 and PM10 readings from the SDS011 sensor.

This repository also explains how to setup your own air filter and which resources
you can follow to get cleaner air in your living area.

Features:

- Build a time-series database.
- Graph the data

## Installing

```sh
git clone github.com/Benehiko/air
cd air
go get -u && go build -o air .
./air
```

To allow the user running this application
access to the device, you need to add a udev rule.
Learn more about udev rules [here](https://wiki.archlinux.org/title/Udev).

```sh
sudo vi /etc/udev/rules.d/71-pm-sensor.rules
```

```
SUBSYSTEMS=="usb", ATTRS{idVendor}=="1a86", ATTRS{idProduct}=="7523", MODE="0660", TAG+="uaccess"
```

## Documentation

I have included a copy of the SDS011 sensor manual
in this GitHub repository under `./docs/sds011-manual.pdf`.

![sds011](./docs/sds011-01.jpg)
![sds011](./docs/sds011-02.jpg)

Paired with this, I have DIY'd an air filter. It doesn't look very pleasant,
but it seems to do the job.

![front fan](./docs/front-fan.jpg)
![back right fan](./docs/back-right-fan.jpg)
![back left fan](./docs/back-left-fan.jpg)

## Aknowledgements

While searching for ways to improve my indoor air quality, I stumbled upon
[smartairfilters by Thomas Talhelm](https://smartairfilters.com/en/blog/how-to-make-diy-air-purifier/).
This helped a lot with understanding the mysteries around air purification and
that it doesn't need to cost a lot!

As for air quality monitoring, I wanted something I could plug into a RaspberryPi
and just have run 24/7. I got some insightful information from [Jeff Geerling](https://www.jeffgeerling.com/blog/2021/airgradient-diy-air-quality-monitor-co2-pm25)
and [Alan Byrne](https://youtu.be/dxVUxYIrawU).

Ultimately I chose to get the SDS011 which is affordable and can be interacted
without the need for another propriatary App.
