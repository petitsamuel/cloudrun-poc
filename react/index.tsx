/**
 * @license
 * SPDX-License-Identifier: Apache-2.0
*/
import {useEffect, useState} from 'react';
import ReactDOM from 'react-dom/client';

function App() {
  const [message, setMessage] = useState('');

  useEffect(() => {
    const generateGreeting = async () => {
      setMessage('Hello, world!');
    };

    generateGreeting();
  }, []);

  return (
    <div>
      <h1>{message}</h1>
    </div>
  );
}

const root = ReactDOM.createRoot(document.getElementById('root'));
root.render(<App />);
