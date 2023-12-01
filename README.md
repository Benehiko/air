# SDS011-Go

An easy to use application to read the SDS011 laser PM2.5 sensor.

Features:

- Build a time-series database.
- Graph the data

Read more about the SDS011 sensor documentation [here](https://ecksteinimg.de/Datasheet/SDS011%20laser%20PM2.5%20sensor%20specification-V1.3.pdf)

To allow the user running this application
access to the device, you need to add a udev rule.
Learn more about udev rules [here](https://wiki.archlinux.org/title/Udev).

```sh
sudo vi /etc/udev/rules.d/71-pm-sensor.rules
```

```
SUBSYSTEMS=="usb", ATTRS{idVendor}=="1a86", ATTRS{idProduct}=="7523", MODE="0660", TAG+="uaccess"
```
