/*
Copyright (c) 2017, UPMC Enterprises
All rights reserved.
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
    * Redistributions of source code must retain the above copyright
      notice, this list of conditions and the following disclaimer.
    * Redistributions in binary form must reproduce the above copyright
      notice, this list of conditions and the following disclaimer in the
      documentation and/or other materials provided with the distribution.
    * Neither the name UPMC Enterprises nor the
      names of its contributors may be used to endorse or promote products
      derived from this software without specific prior written permission.
THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL UPMC ENTERPRISES BE LIABLE FOR ANY
DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
*/

package k8sutil

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Sirupsen/logrus"
)

type csr struct {
	CN    string   `json:"CN,omitempty"`
	Hosts []string `json:"hosts"`
	Key   key      `json:"key"`
	Names []names  `json:"names"`
}

type caconfig struct {
	Signing configSigning `json:"signing"`
}

type configSigning struct {
	Default configDefault `json:"default"`
}

type configDefault struct {
	Usages []string `json:"usages"`
	Expiry string   `json:"expiry"`
}

type key struct {
	Algo string `json:"algo"`
	Size int    `json:"size"`
}

type names struct {
	O  string `json:"O,omitempty"`
	OU string `json:"OU,omitempty"`
	L  string `json:"L,omitempty"`
	C  string `json:"C,omitempty"`
	ST string `json:"ST,omitempty"`
}

// GenerateConfig creates the config for certs
func (k *K8sutil) generateConfig(configDir, certsDir, namespace, clusterName string) error {
	caConfig := caconfig{
		Signing: configSigning{
			Default: configDefault{
				Usages: []string{
					"signing",
					"key encipherment",
					"server auth",
					"client auth",
				},
				Expiry: "8760h",
			},
		},
	}

	caCSR := csr{
		Hosts: []string{
			"localhost",
			fmt.Sprintf("elasticsearch-%s", clusterName),
			fmt.Sprintf("%s.%s", fmt.Sprintf("elasticsearch-%s", clusterName), namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", fmt.Sprintf("elasticsearch-%s", clusterName), namespace),
		},
		Key: key{
			Algo: "rsa",
			Size: 2048,
		},
		Names: []names{
			names{
				C:  "US",
				L:  "Pittsburgh",
				O:  "elasticsearch-operator",
				OU: "k8s",
				ST: "Pennsylvania",
			},
		},
	}

	caConfigJSON, err := json.Marshal(caConfig)
	if err != nil {
		logrus.Error("json Marshal error : ", err)
		return err
	}
	f, err := os.Create(fmt.Sprintf("%s/ca-config.json", configDir))
	_, err = f.Write(caConfigJSON)
	if err != nil {
		logrus.Error("Error creating ca-config.json: ", err)
		return err
	}

	reqCACSRJSON, _ := json.Marshal(caCSR)
	f, err = os.Create(fmt.Sprintf("%s/ca-csr.json", configDir))
	_, err = f.Write(reqCACSRJSON)
	if err != nil {
		logrus.Error("Error creating ca-csr.json: ", err)
		return err
	}

	for k, v := range map[string]string{
		"node":    "req-node-csr.json",
		"sgadmin": "req-sgadmin-csr.json",
		"kibana":  "req-kibana-csr.json",
		"cerebro": "req-cerebro-csr.json",
	} {

		req := csr{
			CN: k,
			Hosts: []string{
				"localhost",
				fmt.Sprintf("%s-%s", k, clusterName),
				fmt.Sprintf("%s.%s", fmt.Sprintf("%s-%s", k, clusterName), namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", fmt.Sprintf("%s-%s", k, clusterName), namespace),
			},
			Key: key{
				Algo: "rsa",
				Size: 2048,
			},
			Names: []names{
				names{
					O:  "autogenerated",
					OU: "elasticsearch cluster",
					L:  "operator",
				},
			},
		}

		configJSON, _ := json.Marshal(req)
		f, err := os.Create(fmt.Sprintf("%s/%s", configDir, v))
		_, err = f.Write(configJSON)
		if err != nil {
			logrus.Error(err)
			return err
		}
	}

	return nil
}

// GenerateCerts creates certs
func (k *K8sutil) GenerateCerts(configDir, certsDir, namespace, clusterName string) error {
	// Remove any existing config/certs
	cleanUp(certsDir)
	cleanUp(configDir)

	// Generate new config
	if err := k.generateConfig(configDir, certsDir, namespace, clusterName); err != nil {
		logrus.Error(err)
		return err
	}

	// Generate CA Cert
	logrus.Info("Creating ca cert...")
	cmdCA1 := exec.Command("cfssl", "gencert", "-initca", fmt.Sprintf("%s/ca-csr.json", configDir))
	cmdCA2 := exec.Command("cfssljson", "-bare", fmt.Sprintf("%s/ca", certsDir))
	if _, err := pipeCommands(cmdCA1, cmdCA2); err != nil {
		logrus.Error(err)
		return err
	}

	// Generate client Certs
	for _, name := range []string{"node", "kibana", "cerebro", "sgadmin"} {

		logrus.Infof("Creating %s cert...", name)
		cmd1 := exec.Command("cfssl", "gencert", "-ca", fmt.Sprintf("%s/ca.pem", certsDir), "-ca-key", fmt.Sprintf("%s/ca-key.pem", certsDir), "-config", fmt.Sprintf("%s/ca-config.json", configDir), "-profile=server", fmt.Sprintf("%s/req-%s-csr.json", configDir, name))
		cmd2 := exec.Command("cfssljson", "-bare", fmt.Sprintf("%s/%s", certsDir, name))
		if _, err := pipeCommands(cmd1, cmd2); err != nil {
			logrus.Error(err)
			return err
		}
	}

	logrus.Info("Converting node to pkcs8...")
	cmdConvertNodePkcs8 := exec.Command("openssl", "pkcs8", "-topk8", "-in", fmt.Sprintf("%s/node-key.pem", certsDir), "-out", fmt.Sprintf("%s/node-key.pkcs8.pem", certsDir), "-nocrypt")
	if out, err := cmdConvertNodePkcs8.Output(); err != nil {
		logrus.Error(string(out), err)
		return err
	}

	logrus.Info("Converting sgadmin to pkcs12...")
	cmdConvertSgadmin := exec.Command("openssl", "pkcs12", "-export", "-inkey", fmt.Sprintf("%s/sgadmin-key.pem", certsDir), "-in", fmt.Sprintf("%s/sgadmin.pem", certsDir), "-out", fmt.Sprintf("%s/sgadmin.pkcs12", certsDir), "-password", "pass:changeit", "-certfile", fmt.Sprintf("%s/ca.pem", certsDir))
	if out, err := cmdConvertSgadmin.Output(); err != nil {
		logrus.Error(string(out), err)
		return err
	}

	logrus.Info("Converting node to pkcs12...")
	cmdConvertNode := exec.Command("openssl", "pkcs12", "-export", "-inkey", fmt.Sprintf("%s/node-key.pem", certsDir), "-in", fmt.Sprintf("%s/node.pem", certsDir), "-out", fmt.Sprintf("%s/node.pkcs12", certsDir), "-password", "pass:changeit", "-certfile", fmt.Sprintf("%s/ca.pem", certsDir))
	if out, err := cmdConvertNode.Output(); err != nil {
		logrus.Error(string(out), err)
		return err
	}

	logrus.Info("Converting ca cert to jks...")
	cmdCAJKS := exec.Command("keytool", "-import", "-file", fmt.Sprintf("%s/ca.pem", certsDir), "-alias", "root-ca", "-keystore", fmt.Sprintf("%s/truststore.jks", certsDir),
		"-storepass", "changeit", "-srcstoretype", "pkcs12", "-noprompt")
	if out, err := cmdCAJKS.Output(); err != nil {
		logrus.Error(string(out), err)
		return err
	}

	logrus.Info("Converting sgadmin cert to jks...")
	cmdSgadminJKS := exec.Command("keytool", "-importkeystore", "-srckeystore", fmt.Sprintf("%s/sgadmin.pkcs12", certsDir), "-srcalias", "1", "-destkeystore", fmt.Sprintf("%s/sgadmin-keystore.jks", certsDir),
		"-storepass", "changeit", "-srcstoretype", "pkcs12", "-srcstorepass", "changeit", "-destalias", "elasticsearch-admin")
	if out, err := cmdSgadminJKS.Output(); err != nil {
		logrus.Error(string(out), err)
		return err
	}

	logrus.Info("Converting node cert to jks...")
	cmdNodeJKS := exec.Command("keytool", "-importkeystore", "-srckeystore", fmt.Sprintf("%s/node.pkcs12", certsDir), "-srcalias", "1", "-destkeystore", fmt.Sprintf("%s/node-keystore.jks", certsDir),
		"-storepass", "changeit", "-srcstoretype", "pkcs12", "-srcstorepass", "changeit", "-destalias", "elasticsearch-node")
	if out, err := cmdNodeJKS.Output(); err != nil {
		logrus.Error(string(out), err)
		return err
	}

	return nil
}

// CertsSecretExists returms true if secret exists
func (k *K8sutil) CertsSecretExists(namespace, clusterName string) bool {
	// Check if cert exists
	if secret, err := k.Kclient.CoreV1().Secrets(namespace).Get(fmt.Sprintf("%s-%s", secretName, clusterName), metav1.GetOptions{}); err != nil {
		return false
	} else if len(secret.Name) > 0 {
		return true
	}

	return false
}

// DeleteCertsSecret deletes the certs secret
func (k *K8sutil) DeleteCertsSecret(namespace, clusterName string) error {
	return k.Kclient.CoreV1().Secrets(namespace).Delete(fmt.Sprintf("%s-%s", secretName, clusterName), &metav1.DeleteOptions{})
}

// CreateCertsSecret creates the certs secrets
func (k *K8sutil) CreateCertsSecret(namespace, clusterName, certsDir string) error {
	// Read certs from disk
	nodeKeyStore, err := ioutil.ReadFile(fmt.Sprintf("%s/node-keystore.jks", certsDir))
	if err != nil {
		logrus.Error("Could not read certs:", err)
		return err
	}

	sgadminKeyStore, err := ioutil.ReadFile(fmt.Sprintf("%s/sgadmin-keystore.jks", certsDir))
	if err != nil {
		logrus.Error("Could not read certs:", err)
		return err
	}

	//TODO return err
	trustStore, _ := ioutil.ReadFile(fmt.Sprintf("%s/truststore.jks", certsDir))
	ca, _ := ioutil.ReadFile(fmt.Sprintf("%s/ca.pem", certsDir))
	caKey, _ := ioutil.ReadFile(fmt.Sprintf("%s/ca-key.pem", certsDir))
	node, _ := ioutil.ReadFile(fmt.Sprintf("%s/node.pem", certsDir))
	nodeKey, _ := ioutil.ReadFile(fmt.Sprintf("%s/node-key.pem", certsDir))
	nodeKeyPkcs8, _ := ioutil.ReadFile(fmt.Sprintf("%s/node-key.pkcs8.pem", certsDir))
	sgadmin, _ := ioutil.ReadFile(fmt.Sprintf("%s/sgadmin.pem", certsDir))
	sgadminKey, _ := ioutil.ReadFile(fmt.Sprintf("%s/sgadmin-key.pem", certsDir))
	kibanaKey, _ := ioutil.ReadFile(fmt.Sprintf("%s/kibana-key.pem", certsDir))
	kibana, _ := ioutil.ReadFile(fmt.Sprintf("%s/kibana.pem", certsDir))
	cerebroKey, _ := ioutil.ReadFile(fmt.Sprintf("%s/cerebro-key.pem", certsDir))
	cerebro, _ := ioutil.ReadFile(fmt.Sprintf("%s/cerebro.pem", certsDir))
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", secretName, clusterName),
		},
		Data: map[string][]byte{
			"node-keystore.jks":    nodeKeyStore,
			"sgadmin-keystore.jks": sgadminKeyStore,
			"truststore.jks":       trustStore,
			"ca.pem":               ca,
			"ca-key.pem":           caKey,
			"node.pem":             node,
			"node-key.pem":         nodeKey,
			"node-key.pkcs8.pem":   nodeKeyPkcs8,
			"sgadmin.pem":          sgadmin,
			"sgadmin-key.pem":      sgadminKey,
			"kibana-key.pem":       kibanaKey,
			"kibana.pem":           kibana,
			"cerebro-key.pem":      cerebroKey,
			"cerebro.pem":          cerebro,
		},
	}

	if _, err = k.Kclient.CoreV1().Secrets(namespace).Create(secret); err != nil {
		logrus.Error("Could not create elastic certs secret: ", err)
		return err
	}

	return nil
}

func cleanUp(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		err = os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
			return err
		}
	}
	return nil
}

// https://gist.github.com/dagoof/1477401
func pipeCommands(commands ...*exec.Cmd) ([]byte, error) {
	for i, command := range commands[:len(commands)-1] {
		out, err := command.StdoutPipe()
		if err != nil {
			return nil, err
		}
		command.Start()
		commands[i+1].Stdin = out
	}
	final, err := commands[len(commands)-1].Output()
	if err != nil {
		return nil, err
	}
	return final, nil
}
